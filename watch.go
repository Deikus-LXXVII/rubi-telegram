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

// Watches let the agent follow new messages: from given chats or people and/or containing given words.
// Every new matching message wakes the Bot that set the watch, with the watch's note. Watches expire (30
// days by default) and the agent removes them when done.
//
// A private message (see privacy.go) only triggers a watch by its chat or sender, and the event then
// carries no text: matching words would reveal its content. Messages in chats closed to the agent (see
// access.go) never trigger anything.

type watch struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	From     []string  `json:"from,omitempty"`
	Keywords []string  `json:"keywords,omitempty"`
	Agent    string    `json:"agent,omitempty"`
	Note     string    `json:"note,omitempty"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Hits     int       `json:"hits"`
	LastHit  time.Time `json:"last_hit,omitzero"`
	Stopped  bool      `json:"stopped,omitempty"`
}

const (
	maxWatches      = 50
	maxWatchTerms   = 20
	defaultWatchDay = 30
	maxWatchDays    = 180
)

func (w *watch) active(now time.Time) bool { return !w.Stopped && now.Before(w.Expires) }

func (s *state) activeWatches(now time.Time) []*watch {
	out := []*watch{}
	for _, w := range s.Watches {
		if w.active(now) {
			out = append(out, w)
		}
	}
	return out
}

// fromMatches checks one "from" entry: a chat or user id, an @username, or a chat title or sender name
// (whole, any case).
func fromMatches(m message, entry string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	if e == "" {
		return false
	}
	if n, err := strconv.ParseInt(e, 10, 64); err == nil {
		return n == m.ChatID || n == m.FromID
	}
	if u, ok := strings.CutPrefix(e, "@"); ok {
		return u == strings.ToLower(m.FromUsername)
	}
	return e == strings.ToLower(strings.TrimSpace(m.ChatTitle)) || e == strings.ToLower(strings.TrimSpace(m.From))
}

// matches reports whether a message triggers the watch.
func (w *watch) matches(m message, private bool) bool {
	if len(w.From) > 0 {
		ok := false
		for _, f := range w.From {
			ok = ok || fromMatches(m, f)
		}
		if !ok {
			return false
		}
	}
	if len(w.Keywords) == 0 {
		return len(w.From) > 0
	}
	if private {
		return false
	}
	hay := strings.ToLower(m.Text)
	for _, k := range w.Keywords {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(hay, k) {
			return true
		}
	}
	return false
}

type watchIn struct {
	Name     string   `json:"name" jsonschema:"short name, e.g. Replies from Anna"`
	From     []string `json:"from,omitempty" jsonschema:"chats or people: ids, @usernames, or exact chat titles / names; any of them"`
	Keywords []string `json:"keywords,omitempty" jsonschema:"words in the text; any of them. With from, both must match"`
	Days     int      `json:"days,omitempty" jsonschema:"how long to watch, default 30, max 180"`
	Agent    string   `json:"agent,omitempty" jsonschema:"your Bot's name as connected to Rubi, so new messages wake you"`
	Note     string   `json:"note,omitempty" jsonschema:"what to do when it triggers (your own note, sent back with each match)"`
	Account  string   `json:"account,omitempty" jsonschema:"which connected Telegram account (as telegram_accounts lists it); default: the default one"`
}

type watchIDIn struct {
	ID      string `json:"id" jsonschema:"a watch id (wch_…) or a reply tracking id (trk_…)"`
	Account string `json:"account,omitempty" jsonschema:"which connected Telegram account (as telegram_accounts lists it); default: the default one"`
}

func cleanTerms(list []string) ([]string, error) {
	out := []string{}
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			if len(s) > 200 {
				return nil, errors.New("keep each chat or word under 200 characters")
			}
			out = append(out, s)
		}
	}
	if len(out) > maxWatchTerms {
		return nil, fmt.Errorf("at most %d chats and %d words per watch", maxWatchTerms, maxWatchTerms)
	}
	return out, nil
}

func (x *integration) requestWatch(ctx context.Context, a *account, in watchIn) (any, error) {
	from, err := cleanTerms(in.From)
	if err != nil {
		return nil, err
	}
	words, err := cleanTerms(in.Keywords)
	if err != nil {
		return nil, err
	}
	if len(from) == 0 && len(words) == 0 {
		return nil, errors.New("give at least one chat, person or word")
	}
	days := in.Days
	if days <= 0 {
		days = defaultWatchDay
	}
	days = min(days, maxWatchDays)
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = strings.Join(append(append([]string{}, from...), words...), ", ")
	}
	now := time.Now().UTC()
	w := &watch{ID: randomID("wch_"), Name: name, From: from, Keywords: words, Agent: strings.TrimSpace(in.Agent),
		Note: strings.TrimSpace(in.Note), Created: now, Expires: now.Add(time.Duration(days) * 24 * time.Hour)}
	if len(w.Note) > 1000 {
		return nil, errors.New("keep the note under 1000 characters")
	}
	if a.Level(kindWatch) == rubiplugin.None {
		return x.addWatch(a, w)
	}
	return a.Submit(ctx, rubiplugin.Request{Kind: kindWatch, Summary: "Watch new Telegram messages: " + name,
		Preview: map[string]any{"watch": name, "from": strings.Join(from, ", "), "words": strings.Join(words, ", "),
			"until": w.Expires.Format("2006-01-02"), "wakes": w.Agent},
		Options: []rubiplugin.Option{{Key: "allow", Label: "Allow"}}, Payload: w})
}

func (x *integration) addWatch(a *account, w *watch) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(a)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if len(st.activeWatches(now)) >= maxWatches {
		return nil, fmt.Errorf("at most %d active watches; remove some first", maxWatches)
	}
	st.prune(now)
	st.Watches = append(st.Watches, w)
	if err := a.SaveState(st); err != nil {
		return nil, err
	}
	a.Audit("watch_started", map[string]any{"watch_id": w.ID, "chats": len(w.From), "words": len(w.Keywords)})
	out := map[string]any{"status": "watching", "watch": w}
	if w.Agent == "" {
		out["note"] = "No agent given: matches go to the Bots subscribed to Telegram events (or the default Bot)."
	}
	return out, nil
}

func (x *integration) listWatches(a *account) (any, error) {
	st, err := x.loadState(a)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tracked := []*tracked{}
	for _, t := range st.Tracked {
		if t.active(now) {
			tracked = append(tracked, t)
		}
	}
	return map[string]any{"watches": st.activeWatches(now), "tracked_replies": tracked}, nil
}

func (x *integration) unwatch(a *account, id string) (any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(a)
	if err != nil {
		return nil, err
	}
	for _, w := range st.Watches {
		if w.ID == id && !w.Stopped {
			w.Stopped = true
			a.Audit("watch_stopped", map[string]any{"watch_id": w.ID})
			return map[string]any{"stopped": true}, a.SaveState(st)
		}
	}
	for _, t := range st.Tracked {
		if t.ID == id && !t.Stopped {
			t.Stopped = true
			return map[string]any{"stopped": true}, a.SaveState(st)
		}
	}
	return map[string]any{"stopped": false}, nil
}

// botConfig is the plugin-wide setting for bots.
type botConfig struct {
	Wake bool `json:"bot_wake"`
}

// incoming handles new messages of one account: replies to tracked messages, watches, and (for a bot,
// if the user allows) waking the agent for every message. The caller saves the state.
func (x *integration) incoming(a *account, st *state, msgs []message, bot bool) {
	now := time.Now()
	p := privacyOf(a)
	wake := botConfig{Wake: true}
	_ = a.Config(&wake)
	for _, m := range msgs {
		if m.Out || x.chatAllowed(a, m.ChatID) != nil {
			continue
		}
		private, _ := p.hidden(m)
		shown := m
		if private {
			shown = p.redact(m)
		}
		ev := eventMessage(shown)
		for _, t := range st.Tracked {
			if !t.active(now) || t.ChatID != m.ChatID || !(m.ReplyTo == t.MessageID || t.Direct) || m.Date.Before(t.SentAt) {
				continue
			}
			t.Replies++
			_, _ = a.EmitTo("", "reply", map[string]any{
				"tracking_id": t.ID,
				"original":    map[string]any{"chat_id": t.ChatID, "chat_title": t.ChatTitle, "message_id": t.MessageID, "sent_at": t.SentAt},
				"message":     ev,
				"hint":        "Read the chat with " + tool("read") + " if you need more context.",
			})
			a.Audit("reply_found", map[string]any{"tracking_id": t.ID})
		}
		matched := false
		for _, w := range st.activeWatches(now) {
			if m.Date.Before(w.Created.Add(-2*time.Minute)) || !w.matches(m, private) {
				continue
			}
			matched = true
			w.Hits++
			w.LastHit = now.UTC()
			_, _ = a.EmitTo(w.Agent, "message", map[string]any{
				"watch":   map[string]any{"id": w.ID, "name": w.Name, "note": w.Note, "hits": w.Hits},
				"message": ev,
				"hint":    "Read the chat with " + tool("read") + " if you need more. Stop the watch with " + tool("unwatch") + " when it's done.",
			})
			a.Audit("watch_matched", map[string]any{"watch_id": w.ID, "private": private})
		}
		if bot && !matched && wake.Wake {
			_, _ = a.EmitTo("", "message", map[string]any{
				"message": ev,
				"hint":    "Someone wrote to your Telegram bot. Answer with " + tool("send") + " (the user approves) if appropriate.",
			})
		}
	}
}

// eventMessage is what an event carries of a message. Text, sender and chat title come from third
// parties (declared untrusted in the manifest).
func eventMessage(m message) map[string]any {
	ev := map[string]any{"chat_id": m.ChatID, "chat_title": m.ChatTitle, "id": m.ID, "from": m.From,
		"from_id": m.FromID, "date": m.Date, "text": m.Text}
	if m.Private {
		ev["private"] = true
		ev["note"] = privateNote()
		delete(ev, "from_id")
	}
	if m.Media != "" {
		ev["media"] = m.Media
	}
	return ev
}

func unmarshalWatch(payload json.RawMessage) (*watch, error) {
	var w watch
	if err := json.Unmarshal(payload, &w); err != nil {
		return nil, err
	}
	return &w, nil
}
