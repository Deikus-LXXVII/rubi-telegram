package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Several accounts. Rubi keeps each connected account's settings, secrets and per-account settings (which
// chats the agent sees) apart; the privacy filter applies to all of them. Each account is either a bot or
// the user's personal account. The plugin works on one account at a time through an *account, which
// narrows the host to it: its settings, its secrets, its part of the plugin state, and approvals and
// events that carry its name.

// base is what the plugin needs from Rubi (*rubiplugin.Host; a fake in tests).
type base interface {
	SettingsFor(account string, v any) error
	SecretFor(account, key string) (string, error)
	SetSecretFor(account, key, value string) error
	ConfigFor(account string, v any) error
	Accounts() ([]rubiplugin.AccountInfo, error)
	LoadState(v any) error
	SaveState(v any) error
	Level(kind string) rubiplugin.Level
	Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error)
	EmitTo(agent, typ string, data map[string]any) (string, error)
	Audit(event string, fields map[string]any)
	Logf(format string, args ...any)
}

// accountIn is the optional account argument every tool takes.
type accountIn struct {
	Account string `json:"account,omitempty" jsonschema:"which connected Telegram account (as telegram_accounts lists it, e.g. @name); default: the default one"`
}

// account is the host narrowed to one connected Telegram account.
type account struct {
	b         base
	id, label string
	isDefault bool
	several   bool // more than one account is connected: name it in approvals
}

// accounts lists the connected accounts, the default one first.
func accounts(b base) ([]*account, error) {
	list, err := b.Accounts()
	if err != nil {
		return nil, err
	}
	def := 0
	for i, a := range list {
		if a.Default {
			def = i
			break
		}
	}
	out := make([]*account, 0, len(list))
	for i, a := range list {
		acc := &account{b: b, id: a.ID, label: a.Label, isDefault: i == def, several: len(list) > 1}
		if acc.label == "" {
			acc.label = a.ID
		}
		if acc.isDefault {
			out = append([]*account{acc}, out...)
		} else {
			out = append(out, acc)
		}
	}
	return out, nil
}

// accountFor finds a connected account by label or id ("" = the default one). "@name" and "name" both
// match "@name (bot)".
func accountFor(b base, ref string) (*account, error) {
	list, err := accounts(b)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no Telegram account is connected")
	}
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return list[0], nil
	}
	short := func(s string) string {
		s = strings.ToLower(s)
		s = strings.TrimSuffix(s, " (bot)")
		return strings.TrimPrefix(s, "@")
	}
	var names []string
	for _, a := range list {
		if ref == strings.ToLower(a.id) || ref == strings.ToLower(a.label) || short(ref) == short(a.label) {
			return a, nil
		}
		names = append(names, a.label)
	}
	return nil, fmt.Errorf("no connected Telegram account %q; connected: %s", ref, strings.Join(names, ", "))
}

func (a *account) Settings(v any) error              { return a.b.SettingsFor(a.id, v) }
func (a *account) Secret(key string) (string, error) { return a.b.SecretFor(a.id, key) }

// SetSecret replaces an account's secret. Only "session" may ever be written: bot_token and api_hash are
// entered by the user and are declared Internal only so setup can ask for them in a later step, so Rubi
// would let the plugin overwrite them. (Rubi will add an explicit flag for plugin-written secrets.)
func (a *account) SetSecret(key, value string) error {
	if key != "session" {
		return fmt.Errorf("the plugin doesn't write the secret %q", key)
	}
	return a.b.SetSecretFor(a.id, key, value)
}
func (a *account) Config(v any) error                 { return a.b.ConfigFor(a.id, v) }
func (a *account) Level(kind string) rubiplugin.Level { return a.b.Level(kind) }
func (a *account) Logf(format string, args ...any)    { a.b.Logf("["+a.label+"] "+format, args...) }

func (a *account) EmitTo(agent, typ string, data map[string]any) (string, error) {
	data["account"] = a.label
	return a.b.EmitTo(agent, typ, data)
}

func (a *account) Audit(event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["account"] = a.id
	a.b.Audit(event, fields)
}

// settings returns the account's stored settings.
func (a *account) settings() (Settings, error) {
	var s Settings
	err := a.Settings(&s)
	return s, err
}

// isBot reports whether the account is a bot (false if its settings can't be read).
func (a *account) isBot() bool {
	s, err := a.settings()
	return err == nil && s.Kind == kindBotAccount
}

// scoped is an approval payload together with the account it belongs to.
type scoped struct {
	Account string          `json:"rubi_account"`
	Data    json.RawMessage `json:"data"`
}

// Submit asks for approval of an action on this account. The account goes with the payload, and the
// preview always names it (a bot and a personal account look very different to the people in a chat).
func (a *account) Submit(ctx context.Context, r rubiplugin.Request) (map[string]any, error) {
	data, err := json.Marshal(r.Payload)
	if err != nil {
		return nil, err
	}
	r.Payload = scoped{Account: a.id, Data: data}
	if a.several {
		r.Summary += " (" + a.label + ")"
	}
	switch p := r.Preview.(type) {
	case map[string]string:
		p["account"] = a.label
	case map[string]any:
		p["account"] = a.label
	}
	return a.b.Submit(ctx, r)
}

// State. The plugin state holds each account's part under "accounts".

type allState struct {
	Accounts map[string]json.RawMessage `json:"accounts"`
}

func (a *account) loadAll() (allState, error) {
	var all allState
	if err := a.b.LoadState(&all); err != nil {
		return allState{}, err
	}
	if all.Accounts == nil {
		all.Accounts = map[string]json.RawMessage{}
	}
	return all, nil
}

func (a *account) LoadState(v any) error {
	all, err := a.loadAll()
	if err != nil {
		return err
	}
	if s, ok := all.Accounts[a.id]; ok {
		return json.Unmarshal(s, v)
	}
	return nil
}

func (a *account) SaveState(v any) error {
	all, err := a.loadAll()
	if err != nil {
		return err
	}
	if all.Accounts[a.id], err = json.Marshal(v); err != nil {
		return err
	}
	// Drop the parts of accounts that were disconnected.
	if list, err := a.b.Accounts(); err == nil && len(list) > 0 {
		known := map[string]bool{}
		for _, x := range list {
			known[x.ID] = true
		}
		for id := range all.Accounts {
			if !known[id] {
				delete(all.Accounts, id)
			}
		}
	}
	return a.b.SaveState(all)
}

// addTool registers a tool that works on one account, picked by the account argument.
func addTool[T any](p *rubiplugin.Plugin, x *integration, name, description string, ref func(T) string,
	fn func(ctx context.Context, a *account, in T) (any, error)) {
	rubiplugin.AddTool(p, tool(name), description,
		func(ctx context.Context, h *rubiplugin.Host, in T) (any, error) {
			a, err := accountFor(x.wrap(h), ref(in))
			if err != nil {
				return nil, err
			}
			return fn(ctx, a, in)
		})
}

// onExecute runs an approved action on the account it was requested for.
func onExecute(p *rubiplugin.Plugin, x *integration, kind string, fn func(ctx context.Context, a *account, option string, payload json.RawMessage) (any, error)) {
	p.OnExecute(kind, func(ctx context.Context, h *rubiplugin.Host, option string, payload json.RawMessage) (any, error) {
		return execute(ctx, x.wrap(h), option, payload, fn)
	})
}

func execute(ctx context.Context, b base, option string, payload json.RawMessage,
	fn func(ctx context.Context, a *account, option string, payload json.RawMessage) (any, error)) (any, error) {
	var sc scoped
	if err := json.Unmarshal(payload, &sc); err != nil || sc.Account == "" {
		return nil, errors.New("the approved action names no account")
	}
	a, err := accountFor(b, sc.Account)
	if err != nil {
		return nil, err
	}
	return fn(ctx, a, option, sc.Data)
}
