package main

import (
	"sort"
	"strconv"
	"time"
)

// state is one account's part of the plugin state. Rubi keeps the whole plugin state encrypted and caps
// it at 1 MiB, so the bot's message store is bounded by count, age and size.
type state struct {
	Watches []*watch   `json:"watches,omitempty"`
	Grants  []*grant   `json:"grants,omitempty"`  // temporary chat access the user approved
	Tracked []*tracked `json:"tracked,omitempty"` // sent messages watched for replies
	Bot     *botStore  `json:"bot,omitempty"`     // bots only: recent updates (a bot can't read history)
}

// grant is temporary access to a closed chat.
type grant struct {
	ChatID int64     `json:"chat_id"`
	Until  time.Time `json:"until"`
}

// tracked is a sent message the user asked to be notified about replies to.
type tracked struct {
	ID        string    `json:"id"`
	ChatID    int64     `json:"chat_id"`
	ChatTitle string    `json:"chat_title"`
	MessageID int       `json:"message_id"`
	Direct    bool      `json:"direct"` // a one-to-one chat: any message from the other side counts
	SentAt    time.Time `json:"sent_at"`
	Expires   time.Time `json:"expires"`
	Stopped   bool      `json:"stopped,omitempty"`
	Replies   int       `json:"replies"`
}

func (t *tracked) active(now time.Time) bool { return !t.Stopped && now.Before(t.Expires) }

// Bounds of the bot's message store.
var (
	botPerChat  = 200
	botTotal    = 2000
	botMaxAge   = 14 * 24 * time.Hour
	botMaxBytes = 300 << 10 // per bot account, so several fit in Rubi's 1 MiB state
	botMaxText  = 2000      // runes kept of each message
)

type botStore struct {
	Offset int64               `json:"offset"` // next getUpdates offset
	Chats  map[string]*botChat `json:"chats"`
}

type botChat struct {
	Chat     chatInfo  `json:"chat"`
	Messages []message `json:"messages"` // oldest first
}

func (b *botStore) chat(id int64) *botChat {
	if b == nil || b.Chats == nil {
		return nil
	}
	return b.Chats[strconv.FormatInt(id, 10)]
}

// add stores a message (an edit replaces the earlier version).
func (b *botStore) add(m message) {
	if b.Chats == nil {
		b.Chats = map[string]*botChat{}
	}
	key := strconv.FormatInt(m.ChatID, 10)
	c := b.Chats[key]
	if c == nil {
		c = &botChat{Chat: chatInfo{ID: m.ChatID, Title: m.ChatTitle, Type: m.ChatType}}
		b.Chats[key] = c
	}
	if m.ChatTitle != "" {
		c.Chat.Title = m.ChatTitle
	}
	if m.ChatID == m.FromID && m.FromUsername != "" { // a private chat is named after the other side
		c.Chat.Username = m.FromUsername
	}
	if m.Date.After(c.Chat.LastDate) {
		c.Chat.LastDate = m.Date
	}
	m.Text, _ = truncate(m.Text, botMaxText)
	for i := range c.Messages {
		if c.Messages[i].ID == m.ID {
			c.Messages[i] = m
			return
		}
	}
	c.Messages = append(c.Messages, m)
	sort.SliceStable(c.Messages, func(i, j int) bool { return c.Messages[i].ID < c.Messages[j].ID })
}

// prune keeps the store within its bounds: per chat, by age, in total and in size (oldest go first).
func (b *botStore) prune(now time.Time) {
	if b == nil {
		return
	}
	type ref struct {
		key string
		m   message
	}
	var all []ref
	for key, c := range b.Chats {
		if len(c.Messages) > botPerChat {
			c.Messages = c.Messages[len(c.Messages)-botPerChat:]
		}
		for _, m := range c.Messages {
			if now.Sub(m.Date) <= botMaxAge {
				all = append(all, ref{key, m})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].m.Date.After(all[j].m.Date) }) // newest first
	size, keep := 0, 0
	for keep < len(all) && keep < botTotal {
		size += len(all[keep].m.Text) + len(all[keep].m.From) + len(all[keep].m.ChatTitle) + 160
		if size > botMaxBytes {
			break
		}
		keep++
	}
	kept := map[string][]message{}
	for _, r := range all[:keep] {
		kept[r.key] = append(kept[r.key], r.m)
	}
	for key, c := range b.Chats {
		msgs := kept[key]
		if len(msgs) == 0 {
			delete(b.Chats, key)
			continue
		}
		sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
		c.Messages = msgs
	}
}

// prune drops watches, grants and tracking that ended more than a week ago.
func (s *state) prune(now time.Time) {
	var ws []*watch
	for _, w := range s.Watches {
		if w.active(now) || now.Before(w.Expires.Add(7*24*time.Hour)) && !w.Stopped {
			ws = append(ws, w)
		}
	}
	s.Watches = ws
	var gs []*grant
	for _, g := range s.Grants {
		if now.Before(g.Until) {
			gs = append(gs, g)
		}
	}
	s.Grants = gs
	var ts []*tracked
	for _, t := range s.Tracked {
		if t.active(now) || now.Before(t.Expires.Add(7*24*time.Hour)) && !t.Stopped {
			ts = append(ts, t)
		}
	}
	s.Tracked = ts
	s.Bot.prune(now)
}

func (x *integration) loadState(a *account) (*state, error) {
	st := &state{}
	if err := a.LoadState(st); err != nil {
		return nil, err
	}
	return st, nil
}
