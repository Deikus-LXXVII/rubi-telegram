package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Settings are stored (encrypted) in the vault for each connected account.
type Settings struct {
	Kind     string `json:"kind"` // "bot" | "user"
	ID       int64  `json:"id"`   // the bot's or the user's Telegram id
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
	// Personal accounts only.
	APIID int    `json:"api_id,omitempty"`
	Phone string `json:"phone,omitempty"`
}

// RubiAccountID gives the account a stable id in Rubi (rubiplugin.AccountIDer): the bot's or the user's
// Telegram id, so a changed @username doesn't make a second account and setting it up again updates it.
func (s Settings) RubiAccountID() string { return s.Kind + ":" + strconv.FormatInt(s.ID, 10) }

const (
	kindBotAccount  = "bot"
	kindUserAccount = "user"
)

// serviceID is Telegram's own account, which sends login codes and security notices. Its messages are
// always hidden from the agent.
const serviceID = 777000

// chatInfo is one chat as the agent sees it. IDs are "marked" like the Bot API's: users positive, basic
// groups negative, channels and supergroups -100…, so a chat has the same id for a bot and a personal
// account.
type chatInfo struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Type     string    `json:"type"` // private | bot | group | supergroup | channel
	Username string    `json:"username,omitempty"`
	Unread   int       `json:"unread,omitempty"`
	LastDate time.Time `json:"last_date,omitzero"`
	Private  bool      `json:"private,omitempty"` // hidden by the privacy filter: no messages are shown
}

// message is one Telegram message.
type message struct {
	ID           int       `json:"id"`
	ChatID       int64     `json:"chat_id"`
	ChatTitle    string    `json:"chat_title,omitempty"`
	ChatType     string    `json:"chat_type,omitempty"`
	FromID       int64     `json:"from_id,omitempty"`
	From         string    `json:"from,omitempty"` // display name
	FromUsername string    `json:"from_username,omitempty"`
	Out          bool      `json:"out,omitempty"` // sent by this account
	Date         time.Time `json:"date"`
	Text         string    `json:"text,omitempty"`
	ReplyTo      int       `json:"reply_to,omitempty"`
	FwdFromID    int64     `json:"fwd_from_id,omitempty"` // a forward: the original sender or chat
	Media        string    `json:"media,omitempty"`       // photo, document, … (not downloaded)
	Private      bool      `json:"private,omitempty"`
}

// fromService reports whether a message comes from Telegram's own account, directly, in its chat, or
// forwarded. Such messages carry Telegram login codes: always hidden, and never revealed.
func (m message) fromService() bool {
	return m.FromID == serviceID || m.ChatID == serviceID || m.FwdFromID == serviceID
}

// selfInfo is the logged-in user or bot.
type selfInfo struct {
	ID       int64
	Username string
	Name     string
	Phone    string
}

// userClient is a connected personal account (MTProto; gotd in production, a fake in tests). It never
// marks anything as read.
type userClient interface {
	Dialogs(ctx context.Context, limit int) ([]chatInfo, error)
	History(ctx context.Context, chatID int64, limit, beforeID int) ([]message, error)
	Messages(ctx context.Context, chatID int64, ids []int) ([]message, error)
	Search(ctx context.Context, query string, chatID int64, limit int) ([]message, error) // chatID 0: all chats
	Send(ctx context.Context, chatID int64, text string, replyTo int) (message, error)
	SaveDraft(ctx context.Context, chatID int64, text string, replyTo int) error
	// Resolve finds a chat by marked id, @username, "me" or exact title.
	Resolve(ctx context.Context, ref string) (chatInfo, error)
	// Done is closed when the connection ends for good (Err says why); nil if it never ends by itself.
	Done() <-chan struct{}
	Err() error
	Close()
}

// userLogin is a personal-account login in progress.
type userLogin interface {
	SendCode(ctx context.Context, phone string) (hash, via string, err error)
	SignIn(ctx context.Context, phone, code, hash string) (selfInfo, error)
	Password(ctx context.Context, password string) (selfInfo, error)
	Session() (string, error) // the authorized session, encoded for storage
	Close()
}

// Login errors (the gotd implementation maps Telegram's errors to these).
var (
	errNeedPassword   = errors.New("two-step verification password needed")
	errBadCode        = errors.New("the code is wrong")
	errCodeExpired    = errors.New("the code expired")
	errBadPassword    = errors.New("the password is wrong")
	errNoAccount      = errors.New("this phone number has no Telegram account")
	errSessionRevoked = errors.New("the Telegram session was ended (logged out from another device?); connect the account again in the Rubi panel")
)

type userConfig struct {
	APIID   int
	APIHash string
	Session string
	// OnSession is called with the new encoded session whenever Telegram's library stores one (a data
	// center migration, a new auth key), so the plugin can save it.
	OnSession func(session string)
}

// Seams for tests.
var (
	newLogin    = gotdLogin
	connectUser = gotdConnect
)

// chatRef is how the agent names a chat: an id (from telegram_chats), @username, or exact title. It
// accepts a JSON number too.
type chatRef string

func (c *chatRef) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = chatRef(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("chat must be an id, @username or title")
	}
	*c = chatRef(n.String())
	return nil
}

func (c chatRef) String() string { return strings.TrimSpace(string(c)) }

// id returns the chat id if the reference is numeric.
func (c chatRef) id() (int64, bool) {
	n, err := strconv.ParseInt(c.String(), 10, 64)
	return n, err == nil
}

func truncate(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}
