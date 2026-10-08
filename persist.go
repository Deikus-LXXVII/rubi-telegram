package main

import (
	"encoding/json"
	"sync"
	"time"
)

// ---- saving renewed sessions ----

// sessionDebounce is how long a renewed MTProto session waits before it is saved, so a burst of changes
// (a migration stores several times) becomes one write to Rubi's vault.
var sessionDebounce = 3 * time.Second

// sessionSaver saves an account's session secret when gotd stores a new one, debounced.
type sessionSaver struct {
	a       *account
	mu      sync.Mutex
	last    string // what Rubi has
	pending string
	timer   *time.Timer
}

func (s *sessionSaver) store(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == "" || v == s.last && s.pending == "" {
		return
	}
	s.pending = v
	if s.timer == nil {
		s.timer = time.AfterFunc(sessionDebounce, s.flush)
	}
}

// flush saves a pending session now.
func (s *sessionSaver) flush() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	v := s.pending
	s.pending = ""
	if v == "" || v == s.last {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err := s.a.SetSecret("session", v); err != nil {
		s.a.Logf("saving the renewed Telegram session failed: %v", err)
		return
	}
	s.mu.Lock()
	s.last = v
	s.mu.Unlock()
	s.a.Audit("session_renewed", nil)
}

// saverFor returns the account's session saver, which knows the session Rubi has stored.
func (x *integration) saverFor(a *account, stored string) *sessionSaver {
	x.cmu.Lock()
	defer x.cmu.Unlock()
	if x.savers == nil {
		x.savers = map[string]*sessionSaver{}
	}
	s := x.savers[a.id]
	if s == nil {
		s = &sessionSaver{a: a}
		x.savers[a.id] = s
	}
	s.mu.Lock()
	if s.pending == "" {
		s.last = stored
	}
	s.mu.Unlock()
	return s
}

// ---- config cache ----

// configTTL bounds how long settings are cached, in case a change notice is missed.
var configTTL = 5 * time.Minute

// cachedBase caches each account's user-only settings (privacy filter, chat access, bot_wake), which are
// read for every incoming message. OnConfigChanged drops them when the user changes something.
type cachedBase struct {
	base
	x *integration
}

type cachedConfig struct {
	raw json.RawMessage
	at  time.Time
}

func (x *integration) wrap(b base) base {
	if c, ok := b.(*cachedBase); ok {
		return c
	}
	return &cachedBase{base: b, x: x}
}

func (c *cachedBase) ConfigFor(account string, v any) error {
	x := c.x
	x.cfgMu.Lock()
	e, ok := x.cfg[account]
	x.cfgMu.Unlock()
	if !ok || time.Since(e.at) > configTTL {
		var raw json.RawMessage
		if err := c.base.ConfigFor(account, &raw); err != nil {
			return err
		}
		e = cachedConfig{raw: raw, at: time.Now()}
		x.cfgMu.Lock()
		if x.cfg == nil {
			x.cfg = map[string]cachedConfig{}
		}
		x.cfg[account] = e
		x.cfgMu.Unlock()
	}
	return json.Unmarshal(e.raw, v)
}

// configChanged drops cached settings: one account's, or all ("" = plugin-wide settings changed).
func (x *integration) configChanged(account string) {
	x.cfgMu.Lock()
	defer x.cfgMu.Unlock()
	if account == "" {
		x.cfg = nil
		return
	}
	delete(x.cfg, account)
	delete(x.cfg, "") // the default account may be this one
}
