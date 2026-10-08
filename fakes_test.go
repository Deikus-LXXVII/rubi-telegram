package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// ---- Rubi ----

// fakeBase stands in for Rubi with several connected accounts. Approvals stay pending; tests execute them.
type fakeBase struct {
	mu       sync.Mutex
	accounts []rubiplugin.AccountInfo
	settings map[string]Settings
	secrets  map[string]map[string]string // account → key → value
	config   map[string]map[string]any    // account → key → value ("" = plugin-wide)
	state    json.RawMessage
	levels   map[string]rubiplugin.Level
	pending  []rubiplugin.Request
	events   []event

	secretWrites int
	configReads  int
}

type event struct {
	typ, agent string
	data       map[string]any
}

func (b *fakeBase) ref(account string) string {
	if account == "" {
		for _, a := range b.accounts {
			if a.Default {
				return a.ID
			}
		}
	}
	return account
}

func (b *fakeBase) SettingsFor(account string, v any) error {
	s, ok := b.settings[b.ref(account)]
	if !ok {
		return errors.New("unknown account")
	}
	raw, _ := json.Marshal(s)
	return json.Unmarshal(raw, v)
}

// SecretFor answers like Rubi: an Internal secret that isn't set comes back empty, without an error.
func (b *fakeBase) SecretFor(account, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range manifest().Secrets {
		if s.Key == key {
			return b.secrets[b.ref(account)][key], nil
		}
	}
	return "", fmt.Errorf("secret %q is not declared in the manifest", key)
}

// SetSecretFor replaces an Internal secret, like Rubi.
func (b *fakeBase) SetSecretFor(account, key, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	internal := false
	for _, s := range manifest().Secrets {
		internal = internal || s.Key == key && s.Internal
	}
	if !internal {
		return fmt.Errorf("secret %q isn't an internal secret", key)
	}
	b.secrets[b.ref(account)][key] = value
	b.secretWrites++
	return nil
}

func (b *fakeBase) secret(account, key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.secrets[account][key]
}

// ConfigFor merges the manifest defaults with what the test set, like Rubi does.
func (b *fakeBase) ConfigFor(account string, v any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configReads++
	cfg := map[string]any{}
	for _, f := range manifest().Config {
		cfg[f.Key] = f.Default
	}
	for k, x := range b.config[""] {
		cfg[k] = x
	}
	for k, x := range b.config[b.ref(account)] {
		cfg[k] = x
	}
	raw, _ := json.Marshal(cfg)
	return json.Unmarshal(raw, v)
}

func (b *fakeBase) setConfig(account, key string, v any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.config == nil {
		b.config = map[string]map[string]any{}
	}
	if b.config[account] == nil {
		b.config[account] = map[string]any{}
	}
	b.config[account][key] = v
}

func (b *fakeBase) Accounts() ([]rubiplugin.AccountInfo, error) { return b.accounts, nil }

func (b *fakeBase) LoadState(v any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		return nil
	}
	return json.Unmarshal(b.state, v)
}

func (b *fakeBase) SaveState(v any) error {
	raw, err := json.Marshal(v)
	if len(raw) > 1<<20 {
		return errors.New("state is larger than 1 MiB")
	}
	b.mu.Lock()
	b.state = raw
	b.mu.Unlock()
	return err
}

func (b *fakeBase) Level(kind string) rubiplugin.Level {
	if l, ok := b.levels[kind]; ok {
		return l
	}
	if a, ok := func() (rubiplugin.Action, bool) { m := manifest(); return m.Action(kind) }(); ok {
		return a.DefaultLevel
	}
	return rubiplugin.Strong
}

func (b *fakeBase) Submit(_ context.Context, r rubiplugin.Request) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, r)
	return map[string]any{"status": "awaiting_approval"}, nil
}

func (b *fakeBase) EmitTo(agent, typ string, data map[string]any) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event{typ: typ, agent: agent, data: data})
	return "evt", nil
}

func (b *fakeBase) Audit(string, map[string]any) {}
func (b *fakeBase) Logf(string, ...any)          {}

func (b *fakeBase) takeEvents() []event {
	b.mu.Lock()
	defer b.mu.Unlock()
	ev := b.events
	b.events = nil
	return ev
}

func (b *fakeBase) takePending(t *testing.T) rubiplugin.Request {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) != 1 {
		t.Fatalf("want 1 pending approval, have %d", len(b.pending))
	}
	r := b.pending[0]
	b.pending = nil
	return r
}

// approve runs a pending approval the way Rubi does: the payload as JSON, the chosen option.
func approve(t *testing.T, b *fakeBase, r rubiplugin.Request, option string,
	fn func(ctx context.Context, a *account, option string, payload json.RawMessage) (any, error)) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r.Payload)
	if err != nil {
		t.Fatal(err)
	}
	res, err := execute(context.Background(), b, option, raw, fn)
	if err != nil {
		t.Fatalf("execute %s: %v", r.Kind, err)
	}
	return asMap(t, res)
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// ---- a personal account ----

// fakeUser is a connected personal account with a few chats.
type fakeUser struct {
	mu        sync.Mutex
	chats     []chatInfo
	msgs      map[int64][]message // chat → messages, oldest first
	sent      []message
	drafts    []message
	nextID    int
	calls     int
	closed    bool
	onMsg     func(message)
	onSession func(string)
	selfID    int64
	history   []string // calls made, e.g. "history 101"
}

func newFakeUser() *fakeUser {
	u := &fakeUser{msgs: map[int64][]message{}, nextID: 1000, selfID: 42}
	u.chats = []chatInfo{
		{ID: 101, Title: "Anna Smith", Type: "private", Username: "anna", Unread: 2},
		{ID: -1001234, Title: "Family", Type: "supergroup", Unread: 5},
		{ID: serviceID, Title: "Telegram", Type: "private", Unread: 1},
		{ID: 202, Title: "Bank Alerts", Type: "private"},
	}
	now := time.Now().UTC()
	u.msgs[101] = []message{
		{ID: 1, ChatID: 101, ChatTitle: "Anna Smith", FromID: 101, From: "Anna Smith", FromUsername: "anna", Date: now.Add(-time.Hour), Text: "Lunch tomorrow?"},
		{ID: 2, ChatID: 101, ChatTitle: "Anna Smith", FromID: 101, From: "Anna Smith", FromUsername: "anna", Date: now.Add(-30 * time.Minute), Text: "Your verification code is 482913"},
	}
	u.msgs[serviceID] = []message{
		{ID: 7, ChatID: serviceID, ChatTitle: "Telegram", FromID: serviceID, From: "Telegram", Date: now, Text: "Login code: 55123. Do not give this code to anyone"},
	}
	u.msgs[-1001234] = []message{
		{ID: 50, ChatID: -1001234, ChatTitle: "Family", FromID: 303, From: "Mom", Date: now, Text: "Dinner at 7"},
	}
	u.msgs[202] = []message{
		{ID: 9, ChatID: 202, ChatTitle: "Bank Alerts", FromID: 202, From: "Bank Alerts", Date: now, Text: "Balance: 1,204.00"},
	}
	return u
}

func (u *fakeUser) Dialogs(_ context.Context, limit int) ([]chatInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	out := append([]chatInfo(nil), u.chats...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (u *fakeUser) History(_ context.Context, chatID int64, limit, beforeID int) ([]message, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.history = append(u.history, "history "+strconv.FormatInt(chatID, 10))
	var out []message
	list := u.msgs[chatID]
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		if beforeID == 0 || list[i].ID < beforeID {
			out = append(out, list[i])
		}
	}
	return out, nil
}

func (u *fakeUser) Messages(_ context.Context, chatID int64, ids []int) ([]message, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []message
	for _, m := range u.msgs[chatID] {
		if want[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (u *fakeUser) Search(_ context.Context, query string, chatID int64, limit int) ([]message, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var keys []int64
	for k := range u.msgs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var out []message
	for _, k := range keys {
		if chatID != 0 && k != chatID {
			continue
		}
		for _, m := range u.msgs[k] {
			if strings.Contains(strings.ToLower(m.Text), strings.ToLower(query)) && len(out) < limit {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (u *fakeUser) Send(_ context.Context, chatID int64, text string, replyTo int) (message, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.nextID++
	m := message{ID: u.nextID, ChatID: chatID, Out: true, Text: text, ReplyTo: replyTo, Date: time.Now().UTC()}
	u.sent = append(u.sent, m)
	return m, nil
}

func (u *fakeUser) SaveDraft(_ context.Context, chatID int64, text string, replyTo int) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.drafts = append(u.drafts, message{ChatID: chatID, Text: text, ReplyTo: replyTo})
	return nil
}

func (u *fakeUser) Resolve(_ context.Context, ref string) (chatInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range u.chats {
		if ref == strconv.FormatInt(c.ID, 10) || strings.EqualFold(ref, "@"+c.Username) && c.Username != "" || strings.EqualFold(ref, c.Title) {
			return c, nil
		}
	}
	return chatInfo{}, fmt.Errorf("no chat %q", ref)
}

func (u *fakeUser) Done() <-chan struct{} { return nil }
func (u *fakeUser) Err() error            { return nil }
func (u *fakeUser) Close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
}

// deliver simulates an incoming message (as gotd's update handler would report it).
func (u *fakeUser) deliver(m message) {
	u.mu.Lock()
	u.msgs[m.ChatID] = append(u.msgs[m.ChatID], m)
	fn := u.onMsg
	u.mu.Unlock()
	fn(m)
}

// useFakeUser makes connectUser return u and checks what it is called with.
func useFakeUser(t *testing.T, u *fakeUser, wantHash, wantSession string) *int {
	t.Helper()
	dials := 0
	old := connectUser
	connectUser = func(_ context.Context, cfg userConfig, onMessage func(message)) (userClient, error) {
		if cfg.APIHash != wantHash || cfg.Session != wantSession || cfg.APIID != testAPIID {
			return nil, fmt.Errorf("unexpected credentials %+v", cfg)
		}
		dials++
		u.mu.Lock()
		u.onMsg = onMessage
		u.onSession = cfg.OnSession
		u.mu.Unlock()
		return u, nil
	}
	t.Cleanup(func() { connectUser = old })
	return &dials
}

// ---- a personal-account login ----

type fakeLogin struct {
	mu        sync.Mutex
	code      string
	password  string // "" = no two-step verification
	self      selfInfo
	signedIn  bool
	closed    bool
	phone     string
	sentCodes int
}

func (l *fakeLogin) SendCode(_ context.Context, phone string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.phone = phone
	l.sentCodes++
	return "hash-1", "to your Telegram app", nil
}

func (l *fakeLogin) SignIn(_ context.Context, phone, code, hash string) (selfInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if hash != "hash-1" || phone != l.phone {
		return selfInfo{}, errors.New("wrong hash or phone")
	}
	if code != l.code {
		return selfInfo{}, errBadCode
	}
	if l.password != "" {
		return selfInfo{}, errNeedPassword
	}
	l.signedIn = true
	return l.self, nil
}

func (l *fakeLogin) Password(_ context.Context, pw string) (selfInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pw != l.password {
		return selfInfo{}, errBadPassword
	}
	l.signedIn = true
	return l.self, nil
}

func (l *fakeLogin) Session() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.signedIn {
		return "", errors.New("not signed in")
	}
	return "c2Vzc2lvbi1ieXRlcw==", nil
}

func (l *fakeLogin) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

func (l *fakeLogin) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

const (
	testAPIID   = 1234567
	testAPIHash = "0123456789abcdef0123456789abcdef"
	testSession = "c2Vzc2lvbi1ieXRlcw=="
	testToken   = "123456789:AAFakeTokenForTestsOnly_abcdefghijklmn"
)

func useFakeLogin(t *testing.T, l *fakeLogin) *int {
	t.Helper()
	logins := 0
	old := newLogin
	newLogin = func(_ context.Context, cfg userConfig) (userLogin, error) {
		if cfg.APIID != testAPIID || cfg.APIHash != testAPIHash {
			return nil, fmt.Errorf("unexpected credentials %+v", cfg)
		}
		logins++
		return l, nil
	}
	t.Cleanup(func() { newLogin = old })
	return &logins
}

// ---- the Bot API ----

// fakeBotAPI is a Bot API server for one bot.
type fakeBotAPI struct {
	t       *testing.T
	mu      sync.Mutex
	updates []map[string]any // served by getUpdates from the requested offset
	sent    []map[string]any
	offsets []int64
	nextID  int
}

func startBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{t: t, nextID: 500}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	old := botAPIBase
	botAPIBase = srv.URL
	t.Cleanup(func() { botAPIBase = old })
	return f
}

func (f *fakeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "getMe":
		reply(map[string]any{"id": 900, "is_bot": true, "first_name": "Rubi Test", "username": "rubi_test_bot"})
	case "getUpdates":
		offset := int64(p["offset"].(float64))
		f.offsets = append(f.offsets, offset)
		var out []map[string]any
		for _, u := range f.updates {
			if int64(u["update_id"].(int)) >= offset {
				out = append(out, u)
			}
		}
		reply(out)
	case "sendMessage":
		f.sent = append(f.sent, p)
		f.nextID++
		reply(map[string]any{"message_id": f.nextID, "date": time.Now().Unix(),
			"chat": map[string]any{"id": p["chat_id"], "type": "private"},
			"from": map[string]any{"id": 900, "is_bot": true, "first_name": "Rubi Test"}, "text": p["text"]})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 404, "description": "Not Found"})
	}
}

// push queues a private message from a user to the bot.
func (f *fakeBotAPI) push(updateID int, chatID int64, msgID int, from, username, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, map[string]any{"update_id": updateID, "message": map[string]any{
		"message_id": msgID, "date": time.Now().Unix(), "text": text,
		"from": map[string]any{"id": chatID, "is_bot": false, "first_name": from, "username": username},
		"chat": map[string]any{"id": chatID, "type": "private", "first_name": from, "username": username},
	}})
}
