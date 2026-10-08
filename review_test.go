package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Fixes from the security review.

func TestServiceMessagesAreNeverRevealed(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")

	// Asking for Telegram's chat is refused before any approval.
	for _, chat := range []chatRef{"777000", "Telegram"} {
		if _, err := x.requestReveal(ctx, me, revealIn{Chat: chat, IDs: []int{7}, Reason: "log in"}); !errors.Is(err, errNoServiceReveal) {
			t.Fatalf("reveal in %s: %v", chat, err)
		}
	}
	// A login code forwarded into another chat: refused too, even mixed with others.
	u.msgs[101] = append(u.msgs[101], message{ID: 8, ChatID: 101, FromID: 101, FwdFromID: serviceID,
		Date: time.Now().UTC(), Text: "Login code: 55123"})
	if _, err := x.requestReveal(ctx, me, revealIn{Chat: "101", IDs: []int{2, 8}, Reason: "log in"}); !errors.Is(err, errNoServiceReveal) {
		t.Fatalf("forwarded code: %v", err)
	}
	if len(b.pending) != 0 {
		t.Fatal("an approval was requested")
	}
	// Even an approval (forged or old) can't execute it.
	for _, p := range []revealPayload{{ChatID: serviceID, IDs: []int{7}}, {ChatID: 101, IDs: []int{8}}} {
		raw, _ := json.Marshal(scoped{Account: meAcct, Data: mustJSON(p)})
		if _, err := execute(ctx, b, "show", raw, x.reveal); !errors.Is(err, errNoServiceReveal) {
			t.Fatalf("executing %+v: %v", p, err)
		}
	}
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func TestForwardedServiceMessageAlwaysHidden(t *testing.T) {
	off := privacyConfig{} // every switch off, and no phrase that a heuristic would catch
	m := message{ID: 1, ChatID: 101, FromID: 101, FwdFromID: serviceID, Text: "55123"}
	if hidden, _ := off.hidden(m); !hidden {
		t.Fatal("a message forwarded from 777000 isn't hidden")
	}
	if off.senderOnly(m) != "Telegram (service notifications)" {
		t.Fatal("sender of a forwarded service message")
	}
	// Both sources report the forward origin.
	var bm botMessage
	_ = json.Unmarshal([]byte(`{"message_id":1,"date":1,"chat":{"id":5,"type":"private"},"from":{"id":5},"text":"55123",
		"forward_origin":{"type":"user","sender_user":{"id":777000,"first_name":"Telegram"}}}`), &bm)
	if got := bm.toMessage(900); got.FwdFromID != serviceID {
		t.Fatalf("bot forward origin: %+v", got)
	}
	_ = json.Unmarshal([]byte(`{"message_id":1,"date":1,"chat":{"id":5,"type":"private"},"forward_from":{"id":777000}}`), &bm)
	if got := (&botMessage{Chat: bm.Chat, ForwardFrom: bm.ForwardFrom}).toMessage(900); got.FwdFromID != serviceID {
		t.Fatalf("legacy forward_from: %+v", got)
	}
}

func TestDraftsNeedApprovalAndAccess(t *testing.T) {
	x, b, u, _ := setupAccounts(t)
	ctx := context.Background()
	me := acct(t, b, "")
	if a, _ := action(kindDraft); a.DefaultLevel != rubiplugin.Chat {
		t.Fatalf("draft level: %v", a.DefaultLevel)
	}
	b.setConfig(meAcct, "chat_access", "selected")
	b.setConfig(meAcct, "chats", []string{"101"})
	if _, err := x.draft(ctx, me, draftIn{Chat: "Family", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("draft in a closed chat: %v", err)
	}
	if _, err := x.draft(ctx, me, draftIn{Chat: "@anna", Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	req := b.takePending(t)
	if req.Kind != kindDraft || !strings.Contains(req.Summary, "replaces") || len(u.drafts) != 0 {
		t.Fatalf("draft request: %+v, drafts %v", req, u.drafts)
	}
}

func TestConfigChangeForOneAccountClearsAll(t *testing.T) {
	x, b, _, _ := setupAccounts(t)
	wb := x.wrap(b)
	me, bot := acct(t, wb, meAcct), acct(t, wb, botAcct)
	m := message{ID: 1, ChatID: 101, FromID: 101, Text: "about the merger"}
	_, _ = privacyOf(me), privacyOf(bot) // both cached

	// A plugin-wide field changes, but Rubi names one account.
	b.setConfig("", "hidden_keywords", []string{"merger"})
	x.configChanged(botAcct)
	for _, a := range []*account{me, bot} {
		if hidden, _ := privacyOf(a).hidden(m); !hidden {
			t.Fatalf("%s still uses the old privacy settings", a.label)
		}
	}
}

func TestOnlySessionIsWritten(t *testing.T) {
	_, b, _, _ := setupAccounts(t)
	me := acct(t, b, "")
	for _, key := range []string{"api_hash", "bot_token"} {
		if err := me.SetSecret(key, "x"); err == nil {
			t.Fatalf("wrote %s", key)
		}
	}
	if err := me.SetSecret("session", "bmV3"); err != nil || b.secret(meAcct, "session") != "bmV3" {
		t.Fatal(err)
	}
}
