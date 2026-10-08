package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Accounts as Rubi keeps them: the stable id from Settings.RubiAccountID, the name as label.
const (
	meAcct   = "user:42"
	meLabel  = "@me_on_tg"
	botAcct  = "bot:900"
	botLabel = "@rubi_test_bot (bot)"
)

// setupAccounts connects a personal account (the default) and a bot, both on fakes.
func setupAccounts(t *testing.T) (*integration, *fakeBase, *fakeUser, *fakeBotAPI) {
	t.Helper()
	u := newFakeUser()
	useFakeUser(t, u, testAPIHash, testSession)
	api := startBotAPI(t)
	b := &fakeBase{
		accounts: []rubiplugin.AccountInfo{{ID: meAcct, Label: meLabel, Default: true}, {ID: botAcct, Label: botLabel}},
		settings: map[string]Settings{
			meAcct:  {Kind: kindUserAccount, ID: 42, Username: "me_on_tg", APIID: testAPIID, Phone: "+447700900123"},
			botAcct: {Kind: kindBotAccount, ID: 900, Username: "rubi_test_bot"},
		},
		secrets: map[string]map[string]string{
			meAcct:  {"api_hash": testAPIHash, "session": testSession},
			botAcct: {"bot_token": testToken},
		},
	}
	x := &integration{}
	t.Cleanup(x.Stop)
	return x, b, u, api
}

func acct(t *testing.T, b base, ref string) *account {
	t.Helper()
	a, err := accountFor(b, ref)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func messagesOf(t *testing.T, res any) []map[string]any {
	t.Helper()
	m := asMap(t, res)
	raw, _ := json.Marshal(m["messages"])
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func TestPrivacyFilter(t *testing.T) {
	// Telegram's own account is hidden even with every switch off.
	off := privacyConfig{}
	if hidden, _ := off.hidden(message{ChatID: serviceID, FromID: serviceID, Text: "hello"}); !hidden {
		t.Fatal("a message from 777000 isn't hidden")
	}
	on := privacyConfig{HideCodes: true, HideResets: true, Chats: []string{"202"}, Keywords: []string{"Diagnosis"}}
	cases := []struct {
		text   string
		chat   int64
		hidden bool
	}{
		{"Your verification code is 482913", 101, true},
		{"482913 is your Steam code", 101, true},
		{"Ваш код: 4821", 101, true},
		{"Use this link to reset your password", 101, true},
		{"New sign-in from Chrome on Windows", 101, false}, // alerts are off by default
		{"Lunch tomorrow at 1230?", 101, false},
		{"Anything at all", 202, true}, // a hidden chat
		{"the diagnosis came back", 101, true},
		{"Meet at the stop", 101, false}, // "otp" only as a whole word
	}
	for _, c := range cases {
		if hidden, why := on.hidden(message{ChatID: c.chat, FromID: c.chat, Text: c.text}); hidden != c.hidden {
			t.Errorf("%q in %d: hidden=%v (%s)", c.text, c.chat, hidden, why)
		}
	}
	if s := on.senderOnly(message{FromID: 5, From: "Code 123456"}); s != "user 5" {
		t.Errorf("a name that looks like a code is shown: %q", s)
	}

	x, b, _, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")

	// Reading a chat: the code message becomes a placeholder; the other stays.
	res, err := x.readChat(ctx, me, readIn{Chat: "@anna"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := messagesOf(t, res)
	if len(msgs) != 2 || msgs[0]["private"] != true || msgs[0]["text"] != "a private message from @anna" ||
		msgs[1]["text"] != "Lunch tomorrow?" || asMap(t, res)["private_note"] == nil {
		t.Fatalf("read: %v", msgs)
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "482913") {
		t.Fatal("the code leaked")
	}
	// Telegram's chat: everything private, and marked so in the chat list.
	res, _ = x.readChat(ctx, me, readIn{Chat: chatRef(fmt.Sprint(serviceID))})
	raw, _ = json.Marshal(res)
	if strings.Contains(string(raw), "55123") || !strings.Contains(string(raw), "Telegram (service notifications)") {
		t.Fatalf("777000 read: %s", raw)
	}
	res, _ = x.chats(ctx, me, chatsIn{})
	for _, c := range asMap(t, res)["chats"].([]any) {
		c := c.(map[string]any)
		if c["id"] == float64(serviceID) && c["private"] != true {
			t.Fatal("Telegram's chat isn't marked private")
		}
	}
	// Search by content says nothing about private messages.
	res, err = x.search(ctx, me, searchIn{Query: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(t, res); len(got) != 0 {
		t.Fatalf("search found private messages: %v", got)
	}
	// The user hides a chat and a word.
	b.setConfig("", "hidden_chats", []string{"-1001234"})
	res, _ = x.readChat(ctx, me, readIn{Chat: "Family"})
	if m := messagesOf(t, res); m[0]["private"] != true || m[0]["text"] == "Dinner at 7" {
		t.Fatalf("hidden chat: %v", m)
	}
}

func TestRevealBatch(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")
	now := time.Now().UTC()
	u.msgs[101] = append(u.msgs[101], message{ID: 3, ChatID: 101, FromID: 101, FromUsername: "anna", Date: now, Text: "Your login code: 1111"})

	res, err := x.requestReveal(ctx, me, revealIn{Chat: "101", IDs: []int{1, 2, 3}, Reason: "the user asked me to enter the code"})
	if err != nil {
		t.Fatal(err)
	}
	if asMap(t, res)["not_private"] == nil {
		t.Fatal("message 1 isn't private and should be pointed out")
	}
	req := b.takePending(t)
	if req.Kind != kindPrivate || len(req.Items) != 2 {
		t.Fatalf("reveal request: %+v", req)
	}
	if a, _ := action(kindPrivate); !a.Locked || a.DefaultLevel != rubiplugin.Strong {
		t.Fatal("revealing must be a locked strong approval")
	}
	out := approve(t, b, req, "items:3", x.reveal)
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "1111") || strings.Contains(string(raw), "482913") {
		t.Fatalf("revealed more or less than chosen: %s", raw)
	}
	// A tampered choice can't reveal a message that wasn't in the request.
	out = approve(t, b, req, "items:1", x.reveal)
	if msgs := out["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("revealed an unrequested message: %v", msgs)
	}
}

func TestChatAccessGrant(t *testing.T) {
	x, b, _, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")
	b.setConfig(meAcct, "chat_access", "selected")
	b.setConfig(meAcct, "chats", []string{"101"})

	if _, err := x.readChat(ctx, me, readIn{Chat: "@anna"}); err != nil {
		t.Fatalf("an allowed chat: %v", err)
	}
	_, err := x.readChat(ctx, me, readIn{Chat: "Family"})
	if err == nil || !strings.Contains(err.Error(), tool("chat_access")) {
		t.Fatalf("a closed chat: %v", err)
	}
	res, _ := x.chats(ctx, me, chatsIn{})
	m := asMap(t, res)
	if len(m["chats"].([]any)) != 1 || m["closed"] != float64(3) {
		t.Fatalf("chat list: %v", m)
	}
	if _, err := x.requestChatAccess(ctx, me, chatAccessIn{Chat: "Family", Reason: "the user asked about dinner"}); err != nil {
		t.Fatal(err)
	}
	req := b.takePending(t)
	if req.Kind != kindChat || req.Preview.(map[string]any)["account"] != meLabel {
		t.Fatalf("access request: %+v", req)
	}
	if a, _ := action(kindChat); !a.Locked || a.DefaultLevel != rubiplugin.Strong {
		t.Fatal("opening a chat must be a locked strong approval")
	}
	grant := approve(t, b, req, "1h", func(ctx context.Context, a *account, o string, p json.RawMessage) (any, error) {
		return x.grantChat(a, o, p)
	})
	if grant["status"] != "allowed" {
		t.Fatalf("grant: %v", grant)
	}
	if _, err := x.readChat(ctx, me, readIn{Chat: "Family"}); err != nil {
		t.Fatalf("after the grant: %v", err)
	}
	// "My agent can't ask": no request at all.
	b.setConfig(meAcct, "chat_requests", "never")
	if _, err := x.requestChatAccess(ctx, me, chatAccessIn{Chat: "Bank Alerts", Reason: "x"}); err == nil {
		t.Fatal("asked although the user forbids it")
	}
}

func TestSendNeedsApproval(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")

	res, err := x.requestSend(ctx, me, sendIn{Chat: "@anna", Text: "See you at 1!", ReplyTo: 1})
	if err != nil {
		t.Fatal(err)
	}
	if asMap(t, res)["status"] != "awaiting_approval" || len(u.sent) != 0 {
		t.Fatal("sent without approval")
	}
	if a, _ := action(kindSend); a.DefaultLevel != rubiplugin.Strong || len(a.Options) != 2 {
		t.Fatalf("send action: %+v", a)
	}
	req := b.takePending(t)
	pv := req.Preview.(map[string]any)
	if req.Kind != kindSend || pv["account"] != meLabel || pv["chat"] != "Anna Smith (@anna)" || pv["text"] != "See you at 1!" {
		t.Fatalf("preview: %+v", req)
	}
	out := approve(t, b, req, "send_track", x.send)
	if out["status"] != "sent" || out["tracking"] == nil || len(u.sent) != 1 || u.sent[0].ReplyTo != 1 || u.sent[0].ChatID != 101 {
		t.Fatalf("send: %v %+v", out, u.sent)
	}

	// Starting the background connects the account; Anna answers.
	if err := x.Start(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.onMsg != nil })
	u.deliver(message{ID: 3000, ChatID: 101, ChatTitle: "Anna Smith", FromID: 101, From: "Anna Smith", FromUsername: "anna",
		Date: time.Now().UTC(), Text: "Great!"})
	ev := b.takeEvents()
	if len(ev) != 1 || ev[0].typ != "reply" || ev[0].data["account"] != meLabel {
		t.Fatalf("reply events: %+v", ev)
	}
	if msg := ev[0].data["message"].(map[string]any); msg["text"] != "Great!" || msg["from"] != "Anna Smith" {
		t.Fatalf("reply: %v", msg)
	}

	// Too long, or empty: refused before asking.
	if _, err := x.requestSend(ctx, me, sendIn{Chat: "@anna", Text: strings.Repeat("a", 4097)}); err == nil {
		t.Fatal("an overlong message was accepted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestWatchWakesAgent(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")

	res, err := x.requestWatch(ctx, me, watchIn{Name: "Parcel", From: []string{"@anna", "Family"}, Keywords: []string{"parcel"},
		Agent: "grok", Note: "tell the user"})
	if err != nil || asMap(t, res)["status"] != "watching" {
		t.Fatalf("watch: %v %v", res, err)
	}
	if err := x.Start(b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { u.mu.Lock(); defer u.mu.Unlock(); return u.onMsg != nil })
	now := time.Now().UTC()
	anna := message{ChatID: 101, ChatTitle: "Anna Smith", FromID: 101, From: "Anna Smith", FromUsername: "anna", Date: now}

	m := anna
	m.ID, m.Text = 10, "Your parcel has arrived"
	u.deliver(m)
	ev := b.takeEvents()
	if len(ev) != 1 || ev[0].typ != "message" || ev[0].agent != "grok" {
		t.Fatalf("events: %+v", ev)
	}
	if msg := ev[0].data["message"].(map[string]any); msg["text"] != "Your parcel has arrived" || ev[0].data["watch"].(map[string]any)["note"] != "tell the user" {
		t.Fatalf("event: %v", ev[0].data)
	}
	// Untrusted fields are declared for the agent.
	et, _ := func() (rubiplugin.EventType, bool) { m := manifest(); return m.Event("message") }()
	if strings.Join(et.Untrusted, ",") != "message.from,message.text,message.chat_title" {
		t.Fatalf("untrusted: %v", et.Untrusted)
	}

	m.ID, m.Text = 11, "No word here"
	u.deliver(m)
	m.ID, m.Text = 12, "parcel code 123456: your verification code" // private: the words can't match
	u.deliver(m)
	m = message{ID: 13, ChatID: 555, ChatTitle: "Stranger", FromID: 555, From: "Stranger", Date: now, Text: "parcel"}
	u.deliver(m)
	if ev := b.takeEvents(); len(ev) != 0 {
		t.Fatalf("unexpected events: %+v", ev)
	}

	// A sender-only watch fires for a private message, without its text.
	if _, err := x.requestWatch(ctx, me, watchIn{From: []string{"101"}, Agent: "grok"}); err != nil {
		t.Fatal(err)
	}
	m = anna
	m.ID, m.Text = 14, "Your verification code is 999111"
	u.deliver(m)
	ev = b.takeEvents()
	raw, _ := json.Marshal(ev[0].data)
	if len(ev) != 1 || strings.Contains(string(raw), "999111") || !strings.Contains(string(raw), `"private":true`) {
		t.Fatalf("private watch event: %s", raw)
	}

	// Closed chats wake nobody. (The settings are cached; Rubi tells the plugin when they change.)
	b.setConfig(meAcct, "chat_access", "selected")
	x.configChanged(meAcct)
	m.ID, m.Text = 15, "parcel"
	u.deliver(m)
	if ev := b.takeEvents(); len(ev) != 0 {
		t.Fatalf("a closed chat woke the agent: %+v", ev)
	}

	// Listing and stopping.
	list := asMap(t, must(x.listWatches(me)))["watches"].([]any)
	if len(list) != 2 {
		t.Fatalf("watches: %v", list)
	}
	id := list[0].(map[string]any)["id"].(string)
	if asMap(t, must(x.unwatch(me, id)))["stopped"] != true {
		t.Fatal("unwatch")
	}
}

func action(kind string) (rubiplugin.Action, bool) {
	m := manifest()
	return m.Action(kind)
}

func must(v any, err error) any {
	if err != nil {
		panic(err)
	}
	return v
}

func TestBotPollingStoresUpdates(t *testing.T) {
	x, b, _, api := setupAccounts(t)
	ctx := context.Background()
	bot := acct(t, b, "rubi_test_bot")
	tg, err := x.botAPIFor(bot)
	if err != nil {
		t.Fatal(err)
	}
	api.push(1, 7001, 1, "Ivan", "ivan", "Hello bot")
	api.push(2, 7001, 2, "Ivan", "ivan", "Are you there?")
	api.push(3, 7002, 1, "Olga", "", "Your code is 123456")

	if err := x.pollBot(ctx, bot, tg, 900, 0); err != nil {
		t.Fatal(err)
	}
	// bot_wake (default on): every message wakes the subscribed Bots; the code one only as private.
	ev := b.takeEvents()
	if len(ev) != 3 || ev[0].agent != "" || ev[0].data["account"] != botLabel {
		t.Fatalf("events: %+v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "123456") {
		t.Fatal("a code reached the agent")
	}
	// The next poll continues after the last update.
	if err := x.pollBot(ctx, bot, tg, 900, 0); err != nil {
		t.Fatal(err)
	}
	if got := api.offsets; len(got) != 2 || got[0] != 0 || got[1] != 4 {
		t.Fatalf("offsets: %v", got)
	}
	if len(b.takeEvents()) != 0 {
		t.Fatal("old updates woke the agent again")
	}

	// The bot reads what it stored, newest first.
	res, err := x.readChat(ctx, bot, readIn{Chat: "@ivan"})
	if err != nil {
		t.Fatal(err)
	}
	if m := messagesOf(t, res); len(m) != 2 || m[0]["text"] != "Are you there?" || m[1]["text"] != "Hello bot" {
		t.Fatalf("bot read: %v", m)
	}
	res, _ = x.readChat(ctx, bot, readIn{Chat: "7001", BeforeID: 2})
	if m := messagesOf(t, res); len(m) != 1 || m[0]["id"] != float64(1) {
		t.Fatalf("before_id: %v", m)
	}
	chats := asMap(t, must(x.chats(ctx, bot, chatsIn{})))["chats"].([]any)
	if len(chats) != 2 {
		t.Fatalf("bot chats: %v", chats)
	}

	// bot_wake off: only watches wake the agent.
	b.setConfig("", "bot_wake", false)
	api.push(4, 7001, 3, "Ivan", "ivan", "ping")
	if err := x.pollBot(ctx, bot, tg, 900, 0); err != nil {
		t.Fatal(err)
	}
	if ev := b.takeEvents(); len(ev) != 0 {
		t.Fatalf("bot_wake off: %+v", ev)
	}

	// Sending as the bot needs approval too, and goes through the Bot API.
	if _, err := x.requestSend(ctx, bot, sendIn{Chat: "@ivan", Text: "Hi Ivan", ReplyTo: 2}); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 0 {
		t.Fatal("the bot sent without approval")
	}
	req := b.takePending(t)
	if !strings.Contains(req.Preview.(map[string]any)["from"].(string), "comes from the bot") {
		t.Fatalf("bot preview: %v", req.Preview)
	}
	out := approve(t, b, req, "send", x.send)
	if out["status"] != "sent" || len(api.sent) != 1 || api.sent[0]["chat_id"] != float64(7001) {
		t.Fatalf("bot send: %v %v", out, api.sent)
	}
}

func TestBotStoreBounds(t *testing.T) {
	oldChat, oldTotal, oldBytes := botPerChat, botTotal, botMaxBytes
	botPerChat, botTotal = 3, 5
	t.Cleanup(func() { botPerChat, botTotal, botMaxBytes = oldChat, oldTotal, oldBytes })
	now := time.Now().UTC()
	s := &botStore{}
	for i := 1; i <= 4; i++ {
		s.add(message{ID: i, ChatID: 1, Date: now.Add(time.Duration(i) * time.Second), Text: "a"})
		s.add(message{ID: i, ChatID: 2, Date: now.Add(time.Duration(i)*time.Second + time.Millisecond), Text: "b"})
	}
	s.add(message{ID: 9, ChatID: 3, Date: now.Add(-15 * 24 * time.Hour), Text: "old"})
	s.prune(now)
	if s.chat(3) != nil {
		t.Fatal("an old message was kept")
	}
	n := len(s.chat(1).Messages) + len(s.chat(2).Messages)
	if n != 5 || len(s.chat(2).Messages) != 3 || s.chat(2).Messages[0].ID != 2 {
		t.Fatalf("bounds: chat1=%v chat2=%v", s.chat(1).Messages, s.chat(2).Messages)
	}
	// The size budget keeps the newest.
	botMaxBytes = 400
	s.prune(now)
	if n := len(s.chat(1).Messages) + len(s.chat(2).Messages); n != 2 {
		t.Fatalf("size budget kept %d", n)
	}
}

func TestSeveralAccounts(t *testing.T) {
	x, b, u, api := setupAccounts(t)
	ctx := context.Background()

	if a := acct(t, b, ""); a.id != meAcct {
		t.Fatalf("default: %s", a.id)
	}
	for _, ref := range []string{"@rubi_test_bot", "rubi_test_bot", " @RUBI_test_bot (bot) "} {
		if a := acct(t, b, ref); a.id != botAcct {
			t.Fatalf("%q: %s", ref, a.id)
		}
	}
	if _, err := accountFor(b, "@nobody"); err == nil || !strings.Contains(err.Error(), botLabel) {
		t.Fatalf("unknown account: %v", err)
	}
	kinds := asMap(t, must(listAccounts(b)))["accounts"].([]any)
	if kinds[0].(map[string]any)["kind"] != "personal" || kinds[1].(map[string]any)["kind"] != "bot" {
		t.Fatalf("accounts: %v", kinds)
	}

	me, bot := acct(t, b, meAcct), acct(t, b, botAcct)
	// Each account uses its own backend.
	if _, err := x.chats(ctx, me, chatsIn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.chats(ctx, bot, chatsIn{}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.search(ctx, bot, searchIn{Query: "x"}); err == nil || !strings.Contains(err.Error(), "bot can't search") {
		t.Fatalf("bot search: %v", err)
	}
	if _, err := x.draft(ctx, bot, draftIn{Chat: "1", Text: "x"}); err == nil {
		t.Fatal("a bot saved a draft")
	}
	if _, err := x.draft(ctx, me, draftIn{Chat: "@anna", Text: "Draft text"}); err != nil || len(u.drafts) != 0 {
		t.Fatalf("draft: %v %v", err, u.drafts)
	}
	approve(t, b, b.takePending(t), "save", func(ctx context.Context, a *account, _ string, p json.RawMessage) (any, error) {
		var d sendPayload
		_ = json.Unmarshal(p, &d)
		return x.saveDraft(ctx, a, d)
	})
	if len(u.drafts) != 1 || u.drafts[0].ChatID != 101 {
		t.Fatalf("approved draft: %v", u.drafts)
	}

	// Approvals name the account; executing goes to the account it was asked for.
	api.push(1, 7001, 1, "Ivan", "ivan", "Hello")
	tg, _ := x.botAPIFor(bot)
	if err := x.pollBot(ctx, bot, tg, 900, 0); err != nil {
		t.Fatal(err)
	}
	b.takeEvents()
	if _, err := x.requestSend(ctx, bot, sendIn{Chat: "7001", Text: "from the bot"}); err != nil {
		t.Fatal(err)
	}
	req := b.takePending(t)
	if !strings.HasSuffix(req.Summary, "("+botLabel+")") {
		t.Fatalf("summary: %s", req.Summary)
	}
	approve(t, b, req, "send", x.send)
	if len(api.sent) != 1 || len(u.sent) != 0 {
		t.Fatal("sent from the wrong account")
	}

	// Watches and grants are per account.
	if _, err := x.requestWatch(ctx, bot, watchIn{Keywords: []string{"order"}}); err != nil {
		t.Fatal(err)
	}
	if w := asMap(t, must(x.listWatches(me)))["watches"].([]any); len(w) != 0 {
		t.Fatalf("the personal account sees the bot's watch: %v", w)
	}
	// Per-account chat access: closing the bot's chats doesn't close the personal ones.
	b.setConfig(botAcct, "chat_access", "selected")
	if _, err := x.readChat(ctx, bot, readIn{Chat: "7001"}); err == nil {
		t.Fatal("a closed bot chat was read")
	}
	if _, err := x.readChat(ctx, me, readIn{Chat: "@anna"}); err != nil {
		t.Fatal(err)
	}
	// Dynamic settings: one account's chats, or every account's for the hidden list.
	opts, err := x.configOptions(ctx, b, botAcct, "chats")
	if err != nil || len(opts) != 1 || opts[0].Key != "7001" {
		t.Fatalf("bot chat options: %v %v", opts, err)
	}
	opts, err = x.configOptions(ctx, b, "", "hidden_chats")
	if err != nil || len(opts) != 4 { // Anna, Family, Bank Alerts, Ivan (not 777000, always hidden)
		t.Fatalf("hidden chat options: %v %v", opts, err)
	}
	// A disconnected account's state is dropped on the next save.
	b.accounts = b.accounts[:1]
	if _, err := x.requestWatch(ctx, me, watchIn{Keywords: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b.state), "7001") {
		t.Fatal("the disconnected bot's messages are still stored")
	}
}

func TestBotTokenNeverInErrors(t *testing.T) {
	api := &botHTTP{base: "http://127.0.0.1:1", token: testToken, hc: &http.Client{Timeout: time.Second}}
	_, err := api.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "AAFake") {
		t.Fatalf("error: %v", err)
	}
}

func TestReadNeedsApprovalWhenUserAsks(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	b.levels = map[string]rubiplugin.Level{kindRead: rubiplugin.Chat}
	me := acct(t, b, "")
	res, err := x.read(context.Background(), me, readPayload{Op: "read", Read: &readIn{Chat: "@anna"}}, "Read a Telegram chat")
	if err != nil || asMap(t, res)["status"] != "awaiting_approval" || len(u.history) != 0 {
		t.Fatalf("read without approval: %v %v %v", res, err, u.history)
	}
	req := b.takePending(t)
	out := approve(t, b, req, "allow", func(ctx context.Context, a *account, _ string, p json.RawMessage) (any, error) {
		var r readPayload
		_ = json.Unmarshal(p, &r)
		return x.doRead(ctx, a, r)
	})
	if out["messages"] == nil {
		t.Fatalf("approved read: %v", out)
	}
}
