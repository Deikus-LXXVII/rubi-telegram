package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Telegram Bot API (https://core.telegram.org/bots/api), over HTTPS. The token is part of every URL,
// so errors never include URLs.

// botAPIBase is where the Bot API lives (tests point it at an httptest server).
var botAPIBase = "https://api.telegram.org"

// botAPI is what the plugin uses of the Bot API (abstracted so tests can count calls).
type botAPI interface {
	GetMe(ctx context.Context) (botUser, error)
	GetUpdates(ctx context.Context, offset int64, timeout int) ([]botUpdate, error)
	SendMessage(ctx context.Context, chatID int64, text string, replyTo int) (botMessage, error)
}

var newBotAPI = func(token string) botAPI {
	return &botHTTP{base: botAPIBase, token: token, hc: &http.Client{}}
}

type botUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

func (u *botUser) name() string {
	if u == nil {
		return ""
	}
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

type botChatInfo struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // private | group | supergroup | channel
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

type botMessage struct {
	MessageID      int          `json:"message_id"`
	From           *botUser     `json:"from,omitempty"`
	SenderChat     *botChatInfo `json:"sender_chat,omitempty"`
	Chat           botChatInfo  `json:"chat"`
	Date           int64        `json:"date"`
	Text           string       `json:"text,omitempty"`
	Caption        string       `json:"caption,omitempty"`
	ReplyToMessage *struct {
		MessageID int `json:"message_id"`
	} `json:"reply_to_message,omitempty"`
	Photo    json.RawMessage `json:"photo,omitempty"`
	Document json.RawMessage `json:"document,omitempty"`
	Voice    json.RawMessage `json:"voice,omitempty"`
	Video    json.RawMessage `json:"video,omitempty"`
	Sticker  json.RawMessage `json:"sticker,omitempty"`
}

type botUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *botMessage `json:"message,omitempty"`
	EditedMessage *botMessage `json:"edited_message,omitempty"`
	ChannelPost   *botMessage `json:"channel_post,omitempty"`
}

// toMessage converts a Bot API message; own is the bot's id (its own messages are "out").
func (m *botMessage) toMessage(own int64) message {
	c := m.Chat
	out := message{ID: m.MessageID, ChatID: c.ID, ChatType: c.Type, Date: time.Unix(m.Date, 0).UTC(), Text: m.Text}
	out.ChatTitle = c.Title
	if c.Type == "private" {
		out.ChatTitle = strings.TrimSpace(c.FirstName + " " + c.LastName)
		if out.ChatTitle == "" && c.Username != "" {
			out.ChatTitle = "@" + c.Username
		}
	}
	if out.Text == "" {
		out.Text = m.Caption
	}
	switch {
	case m.From != nil:
		out.FromID, out.From, out.FromUsername = m.From.ID, m.From.name(), m.From.Username
		out.Out = m.From.ID == own
	case m.SenderChat != nil:
		out.FromID, out.From, out.FromUsername = m.SenderChat.ID, m.SenderChat.Title, m.SenderChat.Username
	}
	if m.ReplyToMessage != nil {
		out.ReplyTo = m.ReplyToMessage.MessageID
	}
	for kind, raw := range map[string]json.RawMessage{"photo": m.Photo, "document": m.Document, "voice": m.Voice,
		"video": m.Video, "sticker": m.Sticker} {
		if len(raw) > 0 {
			out.Media = kind
		}
	}
	return out
}

// botError is an error answer of the Bot API.
type botError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *botError) Error() string {
	if e.Code == http.StatusUnauthorized {
		return "Telegram rejected the bot token (revoked or mistyped?)"
	}
	if e.Code == http.StatusConflict {
		return "Telegram: " + e.Description + " (another program is using this bot, or it has a webhook set)"
	}
	return "Telegram: " + e.Description
}

type botHTTP struct {
	base, token string
	hc          *http.Client
}

func (b *botHTTP) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid bot API request") // the error would contain the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, which holds the token
		}
		return fmt.Errorf("can't reach Telegram: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return fmt.Errorf("Telegram answered %s with something unexpected (HTTP %d)", method, resp.StatusCode)
	}
	if !out.OK {
		code := out.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &botError{Code: code, Description: out.Description, RetryAfter: out.Parameters.RetryAfter}
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(out.Result, result)
}

func (b *botHTTP) GetMe(ctx context.Context) (botUser, error) {
	var u botUser
	err := b.call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

func (b *botHTTP) GetUpdates(ctx context.Context, offset int64, timeout int) ([]botUpdate, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout+20)*time.Second)
	defer cancel()
	var ups []botUpdate
	err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "limit": 100,
		"allowed_updates": []string{"message", "edited_message", "channel_post"}}, &ups)
	return ups, err
}

func (b *botHTTP) SendMessage(ctx context.Context, chatID int64, text string, replyTo int) (botMessage, error) {
	p := map[string]any{"chat_id": chatID, "text": text}
	if replyTo != 0 {
		p["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var m botMessage
	err := b.call(ctx, "sendMessage", p, &m)
	return m, err
}
