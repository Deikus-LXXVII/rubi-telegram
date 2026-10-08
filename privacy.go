package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Privacy filter. Some messages should never reach the agent: Telegram's own login codes (always),
// sign-in codes and password resets from other services, optionally sign-in alerts, and whatever else
// the user adds (hidden chats, hidden words). A hidden message shows up only as "a private message from
// <sender>"; the agent can ask to see one, and the user approves with their passkey or password (the
// reveal tool).
//
// The settings are user-only (manifest config): the user changes them in the Rubi panel with approval,
// and the agent has no way to change them. They apply to every connected account.

type privacyConfig struct {
	HideCodes  bool     `json:"hide_codes"`
	HideResets bool     `json:"hide_password_resets"`
	HideAlerts bool     `json:"hide_sign_in_alerts"`
	Chats      []string `json:"hidden_chats"`
	Keywords   []string `json:"hidden_keywords"`
}

// Built-in phrases by category, matched in lower case.
var (
	codePhrases = []string{
		"verification code", "security code", "confirmation code", "login code", "log-in code", "sign-in code",
		"sign in code", "authentication code", "access code", "one-time code", "one-time password",
		"one time password", "one-time passcode", "passcode", "otp", "2fa", "two-factor", "two-step",
		"magic link", "verify your email", "confirm your email", "verify your identity",
		"do not give this code", "don't give this code", "never share this code", "do not share this code",
		"код подтверждения", "код для входа", "код входа", "проверочный код", "одноразовый код",
		"одноразовый пароль", "код доступа", "код авторизации", "подтверждение входа", "подтвердите адрес",
		"подтвердите почту", "никому не сообщайте", "никому не давайте",
	}
	resetPhrases = []string{
		"password reset", "reset your password", "reset password", "change your password", "password change",
		"сброс пароля", "восстановление пароля", "смена пароля", "изменение пароля", "сбросить пароль",
	}
	alertPhrases = []string{
		"new sign-in", "new sign in", "new login", "new device", "sign in to your account", "suspicious",
		"unusual activity", "unusual sign-in", "security alert", "was your account accessed",
		"новый вход", "вход в аккаунт", "новое устройство", "подозрительн", "необычная активность",
		"оповещение безопасности",
	}
)

var (
	codeWord   = regexp.MustCompile(`(?i)(^|[^\p{L}])(code|codes|pin|код|кода|пин)([^\p{L}]|$)`)
	codeDigits = regexp.MustCompile(`(^|[^\d])\d{4,8}([^\d]|$)`)
)

// hidden reports whether a message must stay hidden from the agent, and why (for the user's eyes only).
func (p privacyConfig) hidden(m message) (bool, string) {
	if m.FromID == serviceID || m.ChatID == serviceID {
		return true, "Telegram service message (login codes)"
	}
	for _, c := range p.Chats {
		if c = strings.TrimSpace(c); c != "" && c == strconv.FormatInt(m.ChatID, 10) {
			return true, "hidden chat"
		}
	}
	text := strings.ToLower(m.Text)
	if len(text) > 20000 {
		text = text[:20000]
	}
	for _, k := range p.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" && strings.Contains(text, k) {
			return true, "hidden word " + k
		}
	}
	if p.HideCodes {
		if anyPhrase(text, codePhrases) {
			return true, "sign-in code or confirmation link"
		}
		// "123456 is your code", "Ваш код: 4821". Only near the start, where such messages put it.
		head, _ := truncate(text, 300)
		if codeWord.MatchString(head) && codeDigits.MatchString(head) {
			return true, "sign-in code or confirmation link"
		}
	}
	if p.HideResets && anyPhrase(text, resetPhrases) {
		return true, "password reset"
	}
	if p.HideAlerts && anyPhrase(text, alertPhrases) {
		return true, "sign-in alert"
	}
	return false, ""
}

// chatHidden reports whether every message of a chat is hidden.
func (p privacyConfig) chatHidden(id int64) bool {
	hidden, _ := p.hidden(message{ChatID: id})
	return hidden
}

func anyPhrase(text string, phrases []string) bool {
	for _, k := range phrases {
		if containsPhrase(text, k) {
			return true
		}
	}
	return false
}

// containsPhrase matches short tokens ("otp", "2fa") as whole words, longer phrases anywhere.
func containsPhrase(text, phrase string) bool {
	if len([]rune(phrase)) > 4 {
		return strings.Contains(text, phrase)
	}
	for i := 0; ; {
		j := strings.Index(text[i:], phrase)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(phrase)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if !wordRune(before) && !wordRune(after) {
			return true
		}
		i = start + 1
	}
}

func wordRune(r rune) bool { return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) }

// senderOnly is what the agent may see of a hidden message's sender: the @username, or the name if it
// doesn't look like a code itself, or the id.
func (p privacyConfig) senderOnly(m message) string {
	switch {
	case m.FromID == serviceID || m.ChatID == serviceID:
		return "Telegram (service notifications)"
	case m.FromUsername != "":
		return "@" + m.FromUsername
	}
	name := strings.TrimSpace(m.From)
	if name != "" {
		if hidden, _ := (privacyConfig{HideCodes: true, HideResets: true}).hidden(message{Text: name}); !hidden {
			return name
		}
	}
	if m.FromID != 0 {
		return "user " + strconv.FormatInt(m.FromID, 10)
	}
	return "(hidden)"
}

// redact replaces a hidden message with what the agent may see of it.
func (p privacyConfig) redact(m message) message {
	return message{ID: m.ID, ChatID: m.ChatID, ChatTitle: m.ChatTitle, ChatType: m.ChatType,
		From: p.senderOnly(m), Out: m.Out, Date: m.Date, Private: true,
		Text: "a private message from " + p.senderOnly(m)}
}

func privacyOf(h *account) privacyConfig {
	p := privacyConfig{HideCodes: true, HideResets: true}
	if err := h.Config(&p); err != nil {
		h.Logf("privacy settings unavailable, hiding codes, resets and alerts: %v", err)
		return privacyConfig{HideCodes: true, HideResets: true, HideAlerts: true}
	}
	return p
}

func privateNote() string {
	return "Some messages are hidden by the user's privacy filter (for example login codes); only the sender is shown. " +
		"If the user needs you to see one, call " + tool("reveal") + "(chat, ids, reason): they approve with their passkey or " +
		"password, and you get it once. Don't ask for codes or passwords otherwise."
}
