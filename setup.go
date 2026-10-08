package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Setup runs in steps (rubiplugin.NeedMore):
//
//	bot:      kind → bot_token → connected ("@name (bot)")
//	personal: kind → api_id, api_hash, phone → code → [2FA password] → connected ("@name" or "+…1234")
//
// Secrets entered in one step aren't sent again in the next, so a personal login in progress (the live
// MTProto connection, the code hash, api_hash) is kept in memory under the step token, for pendingTTL.
// The 2FA password is used once and never stored. What is stored: bot_token for a bot; api_hash and
// the session for a personal account.

const (
	choiceBot  = "bot"
	choiceUser = "user"
)

// pendingTTL is how long a setup step waits for the user.
var pendingTTL = 10 * time.Minute

type pendingLogin struct {
	stage   string // "credentials" | "code" | "password"
	apiID   int
	apiHash string
	phone   string
	hash    string
	login   userLogin
	expires time.Time
	timer   *time.Timer
}

type logins struct {
	mu sync.Mutex
	m  map[string]*pendingLogin
}

// put stores a pending login under a new step token, closing it if the user doesn't come back in time.
func (l *logins) put(p *pendingLogin) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*pendingLogin{}
	}
	step := randomID("stp_")
	p.expires = time.Now().Add(pendingTTL)
	p.timer = time.AfterFunc(pendingTTL, func() {
		l.mu.Lock()
		cur := l.m[step]
		if cur == p {
			delete(l.m, step)
		}
		l.mu.Unlock()
		if cur == p && p.login != nil {
			p.login.Close()
		}
	})
	l.m[step] = p
	return step
}

// take removes and returns the pending login of a step.
func (l *logins) take(step string) (*pendingLogin, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.m[step]
	delete(l.m, step)
	if p == nil {
		return nil, errors.New("this setup step expired (or Rubi restarted); please start again")
	}
	p.timer.Stop()
	if time.Now().After(p.expires) {
		if p.login != nil {
			p.login.Close()
		}
		return nil, errors.New("this setup step expired; please start again")
	}
	return p, nil
}

// closeAll ends every login in progress.
func (l *logins) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for step, p := range l.m {
		p.timer.Stop()
		if p.login != nil {
			p.login.Close()
		}
		delete(l.m, step)
	}
}

var kindField = rubiplugin.Field{Key: "kind", Label: "What do you want to connect?", Type: "choice", Required: true,
	Options: []rubiplugin.Option{
		{Key: choiceBot, Label: "A bot (token from @BotFather)"},
		{Key: choiceUser, Label: "My personal account"},
	}}

var botTokenSecret = rubiplugin.Secret{Key: "bot_token", Label: "Bot token", Internal: true,
	Help: "In Telegram, open @BotFather, send /newbot (or /mybots to pick an existing bot, then API Token), " +
		"and paste the token here. It looks like 123456789:AA… You can revoke it in @BotFather at any time.",
	HelpURL: "https://t.me/BotFather", HelpLink: "Open @BotFather"}

var apiHashSecret = rubiplugin.Secret{Key: "api_hash", Label: "api_hash", Internal: true,
	Help:    "From my.telegram.org > API development tools, next to api_id. Never share it.",
	HelpURL: "https://my.telegram.org", HelpLink: "Open my.telegram.org"}

var sessionSecret = rubiplugin.Secret{Key: "session", Label: "Telegram session", Internal: true,
	Help: "Filled in by Rubi after you log in."}

const personalIntro = "To connect your personal account, Rubi needs your own Telegram API keys. " +
	"Open my.telegram.org, log in with your phone number, choose \"API development tools\" and create an app " +
	"(any name, e.g. \"Rubi\"; platform \"Other\"). Copy api_id and api_hash here. There is one pair per " +
	"phone number; never share them. Note: Telegram watches accounts that use unofficial clients and may " +
	"limit or ban accounts that behave like spam bots, so Rubi acts slowly, never in bulk, and only with " +
	"your approval. Telegram will then send you a login code."

var (
	botTokenRe = regexp.MustCompile(`^\d{5,}:[A-Za-z0-9_-]{30,}$`)
	phoneClean = regexp.MustCompile(`[^\d]`)
)

func (x *integration) Validate(ctx context.Context, fields, secrets map[string]string) (any, string, error) {
	if step := fields[rubiplugin.StepField]; step != "" && fields["kind"] != choiceBot {
		return x.validateUserStep(ctx, step, fields, secrets)
	}
	switch fields["kind"] {
	case choiceBot:
		return x.validateBot(ctx, secrets)
	case choiceUser:
		step := x.logins.put(&pendingLogin{stage: "credentials"})
		return nil, "", &rubiplugin.NeedMore{Message: personalIntro, Step: step,
			Fields: []rubiplugin.Field{
				{Key: "api_id", Label: "api_id", Type: "number", Required: true, Placeholder: "1234567",
					Help: "The number from my.telegram.org > API development tools."},
				{Key: "phone", Label: "Your phone number", Type: "tel", Required: true, Placeholder: "+44 7700 900123",
					Help: "With the country code. Telegram sends the login code to your Telegram app (or by SMS)."},
			},
			Secrets: []rubiplugin.Secret{asked(apiHashSecret)}}
	}
	return nil, "", errors.New("choose a bot or your personal account")
}

func (x *integration) validateBot(ctx context.Context, secrets map[string]string) (any, string, error) {
	token := strings.Join(strings.Fields(secrets["bot_token"]), "")
	if token == "" {
		return nil, "", &rubiplugin.NeedMore{Step: randomID("stp_"),
			Message: "Create a bot with @BotFather in Telegram (or pick one of yours) and paste its token. " +
				"Only you and Rubi will have it; your agent never sees it.",
			Secrets: []rubiplugin.Secret{asked(botTokenSecret)}}
	}
	if !botTokenRe.MatchString(token) {
		return nil, "", errors.New("that doesn't look like a bot token (it looks like 123456789:AA…); copy it again from @BotFather")
	}
	me, err := newBotAPI(token).GetMe(ctx)
	if err != nil {
		return nil, "", err
	}
	if !me.IsBot || me.Username == "" {
		return nil, "", errors.New("Telegram says this token doesn't belong to a bot")
	}
	clearSecrets(secrets)
	secrets["bot_token"] = token
	s := Settings{Kind: kindBotAccount, ID: me.ID, Username: me.Username, Name: me.name()}
	return s, "@" + me.Username + " (bot)", nil
}

func (x *integration) validateUserStep(ctx context.Context, step string, fields, secrets map[string]string) (any, string, error) {
	p, err := x.logins.take(step)
	if err != nil {
		return nil, "", err
	}
	switch p.stage {
	case "credentials":
		return x.sendCode(ctx, p, fields, secrets)
	case "code":
		code := phoneClean.ReplaceAllString(secrets["code"]+fields["code"], "")
		if code == "" {
			return nil, "", x.askCode(p, "Enter the code Telegram sent you.")
		}
		self, err := p.login.SignIn(ctx, p.phone, code, p.hash)
		switch {
		case errors.Is(err, errNeedPassword):
			p.stage = "password"
			return nil, "", x.askPassword(p, "Your account has two-step verification. Enter your Telegram cloud password. "+
				"Rubi uses it once to log in and doesn't keep it.")
		case errors.Is(err, errBadCode):
			return nil, "", x.askCode(p, "That code is wrong. Check the latest message from Telegram and try again.")
		case err != nil:
			p.login.Close()
			if errors.Is(err, errCodeExpired) {
				return nil, "", errors.New("the code expired; please start again")
			}
			return nil, "", err
		}
		return x.finishUser(p, self, secrets)
	case "password":
		pw := secrets["password"]
		delete(secrets, "password")
		if pw == "" {
			return nil, "", x.askPassword(p, "Enter your Telegram two-step verification password.")
		}
		self, err := p.login.Password(ctx, pw)
		if errors.Is(err, errBadPassword) {
			return nil, "", x.askPassword(p, "That password is wrong. Try again (it is your Telegram cloud password, not a code).")
		}
		if err != nil {
			p.login.Close()
			return nil, "", err
		}
		return x.finishUser(p, self, secrets)
	}
	return nil, "", errors.New("unknown setup step; please start again")
}

func (x *integration) sendCode(ctx context.Context, p *pendingLogin, fields, secrets map[string]string) (any, string, error) {
	id, err := strconv.Atoi(strings.TrimSpace(fields["api_id"]))
	if err != nil || id <= 0 {
		return nil, "", errors.New("api_id must be the number from my.telegram.org")
	}
	hash := strings.TrimSpace(secrets["api_hash"])
	if !regexp.MustCompile(`^[0-9a-fA-F]{32}$`).MatchString(hash) {
		return nil, "", errors.New("api_hash must be the 32 letters and digits from my.telegram.org")
	}
	phone := "+" + phoneClean.ReplaceAllString(fields["phone"], "")
	if len(phone) < 8 {
		return nil, "", errors.New("enter your phone number with the country code, e.g. +44 7700 900123")
	}
	login, err := newLogin(ctx, userConfig{APIID: id, APIHash: hash})
	if err != nil {
		return nil, "", fmt.Errorf("can't reach Telegram: %w", err)
	}
	codeHash, via, err := login.SendCode(ctx, phone)
	if err != nil {
		login.Close()
		return nil, "", err
	}
	p.stage, p.apiID, p.apiHash, p.phone, p.hash, p.login = "code", id, hash, phone, codeHash, login
	msg := "Telegram sent a login code"
	if via != "" {
		msg += " " + via
	}
	return nil, "", x.askCode(p, msg+". Enter it here. Never send this code to anyone, including your agent.")
}

func (x *integration) askCode(p *pendingLogin, msg string) error {
	return &rubiplugin.NeedMore{Message: msg, Step: x.logins.put(p),
		Secrets: []rubiplugin.Secret{{Key: "code", Label: "Login code", Help: "The digits Telegram sent you."}}}
}

func (x *integration) askPassword(p *pendingLogin, msg string) error {
	return &rubiplugin.NeedMore{Message: msg, Step: x.logins.put(p),
		Secrets: []rubiplugin.Secret{{Key: "password", Label: "Two-step verification password",
			Help: "Used once to log in; Rubi doesn't store it."}}}
}

func (x *integration) finishUser(p *pendingLogin, self selfInfo, secrets map[string]string) (any, string, error) {
	sess, err := p.login.Session()
	p.login.Close()
	if err != nil {
		return nil, "", fmt.Errorf("logged in, but couldn't save the session: %w", err)
	}
	clearSecrets(secrets)
	secrets["api_hash"], secrets["session"] = p.apiHash, sess
	s := Settings{Kind: kindUserAccount, ID: self.ID, Username: self.Username, Name: self.Name, APIID: p.apiID, Phone: p.phone}
	label := "@" + self.Username
	if self.Username == "" {
		digits := phoneClean.ReplaceAllString(p.phone, "")
		label = "+…" + digits[max(0, len(digits)-4):]
	}
	return s, label, nil
}

// asked is a manifest secret as a setup step asks for it.
func asked(s rubiplugin.Secret) rubiplugin.Secret {
	s.Internal = false
	return s
}

// clearSecrets leaves only what is stored: no codes or passwords from the setup steps.
func clearSecrets(secrets map[string]string) {
	for k := range secrets {
		delete(secrets, k)
	}
	for _, k := range []string{"bot_token", "api_hash", "session"} {
		secrets[k] = ""
	}
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
