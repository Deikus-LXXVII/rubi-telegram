package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Chat access. For each account the user chooses which chats the agent can see (all, or only some), and
// whether the agent may ask for the others; access the user approves (passkey or password) lasts an hour
// or a day. These settings are user-only: the agent can't change them. Sending isn't reading, so it
// works in any chat (with its own approval).

type accessConfig struct {
	Access   string   `json:"chat_access"`   // "all" | "selected"
	Chats    []string `json:"chats"`         // allowed chat ids when Access is "selected"
	Requests string   `json:"chat_requests"` // "ask" | "never"
}

var accessSettings = []rubiplugin.ConfigField{
	{Key: "chat_access", Label: "Chats your agent can see", Type: "choice", Default: "all", PerAccount: true,
		Options: []rubiplugin.Option{{Key: "all", Label: "All chats"}, {Key: "selected", Label: "Only the chats checked below"}}},
	{Key: "chats", Label: "Allowed chats", Type: "list", Dynamic: true, Default: []string{}, PerAccount: true,
		Help: "Used when \"Only the chats checked below\" is chosen. For a bot, the chats that have written to it."},
	{Key: "chat_requests", Label: "Other chats", Type: "choice", Default: "ask", PerAccount: true,
		Options: []rubiplugin.Option{
			{Key: "ask", Label: "My agent may ask; I approve with my passkey or password"},
			{Key: "never", Label: "My agent can't ask"}}},
}

func accessOf(a *account) accessConfig {
	c := accessConfig{Access: "all", Requests: "ask"}
	if err := a.Config(&c); err != nil {
		a.Logf("chat access settings unavailable, closing all chats: %v", err)
		return accessConfig{Access: "selected", Requests: "never"}
	}
	return c
}

// chatAllowed returns nil if the agent may read the chat, or an explanation it can act on.
func (x *integration) chatAllowed(a *account, chatID int64) error {
	c := accessOf(a)
	if c.Access != "selected" {
		return nil
	}
	id := strconv.FormatInt(chatID, 10)
	for _, allowed := range c.Chats {
		if strings.TrimSpace(allowed) == id {
			return nil
		}
	}
	if st, err := x.loadState(a); err == nil {
		now := time.Now()
		for _, g := range st.Grants {
			if g.ChatID == chatID && now.Before(g.Until) {
				return nil
			}
		}
	}
	if c.Requests == "never" {
		return fmt.Errorf("the user doesn't let you access the chat %d", chatID)
	}
	return fmt.Errorf("the chat %d is closed to you. If you need it, call "+tool("chat_access")+"(chat, reason): "+
		"the user approves with their passkey or password", chatID)
}

type chatAccessIn struct {
	Chat    chatRef `json:"chat" jsonschema:"chat id, @username or exact title"`
	Reason  string  `json:"reason" jsonschema:"why you need it, shown to the user"`
	Account string  `json:"account,omitempty" jsonschema:"which connected Telegram account (as telegram_accounts lists it); default: the default one"`
}

type chatGrantPayload struct {
	ChatID int64  `json:"chat_id"`
	Title  string `json:"title"`
}

func (x *integration) requestChatAccess(ctx context.Context, a *account, in chatAccessIn) (any, error) {
	if in.Chat.String() == "" || strings.TrimSpace(in.Reason) == "" {
		return nil, errors.New("give the chat and why you need it (reason)")
	}
	chat, err := x.resolve(ctx, a, in.Chat)
	if err != nil {
		return nil, err
	}
	err = x.chatAllowed(a, chat.ID)
	if err == nil {
		return map[string]any{"status": "already_allowed"}, nil
	}
	if accessOf(a).Requests == "never" {
		return nil, err
	}
	return a.Submit(ctx, rubiplugin.Request{Kind: kindChat,
		Summary: "Let your agent read the chat " + chat.Title,
		Preview: map[string]any{"chat": chatLabel(chat), "reason": strings.TrimSpace(in.Reason)},
		Options: []rubiplugin.Option{{Key: "1h", Label: "Allow for 1 hour"}, {Key: "24h", Label: "Allow for 24 hours"}},
		Payload: chatGrantPayload{ChatID: chat.ID, Title: chat.Title}})
}

func (x *integration) grantChat(a *account, option string, payload json.RawMessage) (any, error) {
	var in chatGrantPayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	d := time.Hour
	if option == "24h" {
		d = 24 * time.Hour
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(a)
	if err != nil {
		return nil, err
	}
	until := time.Now().Add(d).UTC()
	st.prune(time.Now())
	st.Grants = append(st.Grants, &grant{ChatID: in.ChatID, Until: until})
	if err := a.SaveState(st); err != nil {
		return nil, err
	}
	a.Audit("chat_granted", map[string]any{"chat_id": in.ChatID, "until": until})
	return map[string]any{"status": "allowed", "chat_id": in.ChatID, "title": in.Title, "until": until}, nil
}

func chatLabel(c chatInfo) string {
	s := c.Title
	if c.Username != "" {
		s += " (@" + c.Username + ")"
	}
	if s == "" {
		s = strconv.FormatInt(c.ID, 10)
	}
	return s
}

// configOptions lists chats for the dynamic settings: "chats" for one account, "hidden_chats" for all.
func (x *integration) configOptions(ctx context.Context, b base, ref, key string) ([]rubiplugin.Option, error) {
	var list []*account
	switch key {
	case "chats":
		a, err := accountFor(b, ref)
		if err != nil {
			return nil, err
		}
		list = []*account{a}
	case "hidden_chats":
		all, err := accounts(b)
		if err != nil {
			return nil, err
		}
		list = all
	default:
		return nil, nil
	}
	seen := map[int64]bool{}
	out := []rubiplugin.Option{}
	for _, a := range list {
		chats, err := x.allChats(ctx, a, 100)
		if err != nil {
			if len(list) == 1 {
				return nil, err
			}
			a.Logf("listing chats for settings failed: %v", err)
			continue
		}
		for _, c := range chats {
			if seen[c.ID] || c.ID == serviceID {
				continue
			}
			seen[c.ID] = true
			label := chatLabel(c)
			if len(list) > 1 {
				label += " - " + a.label
			}
			out = append(out, rubiplugin.Option{Key: strconv.FormatInt(c.ID, 10), Label: label})
		}
	}
	return out, nil
}
