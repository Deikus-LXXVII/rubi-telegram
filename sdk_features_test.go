package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

func TestStableAccountID(t *testing.T) {
	var s any = Settings{Kind: kindBotAccount, ID: 900, Username: "renamed_bot"}
	ider, ok := s.(rubiplugin.AccountIDer)
	if !ok || ider.RubiAccountID() != "bot:900" {
		t.Fatalf("bot account id: %v", s)
	}
	// The id stays when the @username changes, and a person and a bot never share one.
	if (Settings{Kind: kindUserAccount, ID: 42, Username: "old"}).RubiAccountID() != (Settings{Kind: kindUserAccount, ID: 42, Username: "new"}).RubiAccountID() ||
		(Settings{Kind: kindUserAccount, ID: 900}).RubiAccountID() == ider.RubiAccountID() {
		t.Fatal("account ids aren't stable or collide")
	}
}

func TestRenewedSessionIsSaved(t *testing.T) {
	old := sessionDebounce
	sessionDebounce = 30 * time.Millisecond
	t.Cleanup(func() { sessionDebounce = old })
	x, b, u, _ := setupAccounts(t)
	me := acct(t, x.wrap(b), "")
	if _, err := x.user(context.Background(), me); err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	renew := u.onSession
	u.mu.Unlock()
	if renew == nil {
		t.Fatal("the connection isn't told where to report new sessions")
	}

	renew(testSession) // unchanged: nothing to write
	renew("bmV3LTE=")
	renew("bmV3LTI=") // a burst becomes one write of the latest
	waitFor(t, func() bool { return b.secret(meAcct, "session") == "bmV3LTI=" })
	time.Sleep(60 * time.Millisecond)
	if b.secretWrites != 1 {
		t.Fatalf("%d writes for one burst", b.secretWrites)
	}
	renew("bmV3LTI=")
	time.Sleep(60 * time.Millisecond)
	if b.secretWrites != 1 {
		t.Fatal("an unchanged session was written again")
	}

	// Stopping saves a session that is still waiting.
	sessionDebounce = time.Hour
	renew("bmV3LTM=")
	x.Stop()
	if b.secret(meAcct, "session") != "bmV3LTM=" || b.secretWrites != 2 {
		t.Fatalf("pending session lost on stop: %q (%d writes)", b.secret(meAcct, "session"), b.secretWrites)
	}
}

func TestMissingSessionAsksToReconnect(t *testing.T) {
	x, b, _, _ := setupAccounts(t)
	b.secrets[meAcct]["session"] = "" // Rubi returns "" for an unset internal secret
	if _, err := x.user(context.Background(), acct(t, b, "")); !errors.Is(err, errSessionRevoked) {
		t.Fatalf("missing session: %v", err)
	}
	b.secrets[botAcct]["bot_token"] = ""
	if _, err := x.botAPIFor(acct(t, b, botAcct)); err == nil {
		t.Fatal("a bot without a token was used")
	}
}

func TestConfigChangedRefreshesCache(t *testing.T) {
	x, b, _, _ := setupAccounts(t)
	wb := x.wrap(b)
	me := acct(t, wb, "")
	m := message{ID: 1, ChatID: 101, FromID: 101, Text: "about the merger"}

	if hidden, _ := privacyOf(me).hidden(m); hidden {
		t.Fatal("hidden before the user hid anything")
	}
	reads := b.configReads
	for i := 0; i < 5; i++ {
		_ = privacyOf(me)
		_ = accessOf(me)
	}
	if b.configReads != reads {
		t.Fatalf("settings read %d more times; they should be cached", b.configReads-reads)
	}

	// The user hides a word in the panel; Rubi calls OnConfigChanged (plugin-wide: account "").
	b.setConfig("", "hidden_keywords", []string{"merger"})
	p := newPlugin(x)
	p.OnConfigChanged(nil, "")
	if hidden, _ := privacyOf(me).hidden(m); !hidden {
		t.Fatal("the new hidden word isn't applied")
	}
	// One account's chat access changes.
	b.setConfig(meAcct, "chat_access", "selected")
	p.OnConfigChanged(nil, meAcct)
	if x.chatAllowed(me, 101) == nil {
		t.Fatal("the closed chat is still open")
	}
}
