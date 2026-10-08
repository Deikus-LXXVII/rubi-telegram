package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// declaredSecrets is what Rubi sends to Validate: every manifest secret, empty unless entered now.
func declaredSecrets(entered map[string]string) map[string]string {
	out := map[string]string{}
	for _, s := range manifest().Secrets {
		out[s.Key] = ""
	}
	for k, v := range entered {
		out[k] = v
	}
	return out
}

func needMore(t *testing.T, err error) *rubiplugin.NeedMore {
	t.Helper()
	var more *rubiplugin.NeedMore
	if !errors.As(err, &more) {
		t.Fatalf("want NeedMore, got %v", err)
	}
	if more.Step == "" {
		t.Fatal("NeedMore without a step")
	}
	return more
}

func hasInput(more *rubiplugin.NeedMore, key string) bool {
	for _, f := range more.Fields {
		if f.Key == key {
			return true
		}
	}
	for _, s := range more.Secrets {
		if s.Key == key && !s.Internal {
			return true
		}
	}
	return false
}

func TestSetupBot(t *testing.T) {
	startBotAPI(t)
	x := &integration{}
	ctx := context.Background()
	fields := map[string]string{"kind": choiceBot}

	_, _, err := x.Validate(ctx, fields, declaredSecrets(nil))
	more := needMore(t, err)
	if !hasInput(more, "bot_token") || len(more.Fields) != 0 {
		t.Fatalf("first step should ask for the token only: %+v", more)
	}

	fields[rubiplugin.StepField] = more.Step
	if _, _, err := x.Validate(ctx, fields, declaredSecrets(map[string]string{"bot_token": "nonsense"})); err == nil {
		t.Fatal("a malformed token was accepted")
	}
	if _, _, err := x.Validate(ctx, fields, declaredSecrets(map[string]string{"bot_token": "123456789:AAWrongTokenWrongTokenWrongToken_xyz"})); err == nil ||
		!strings.Contains(err.Error(), "rejected the bot token") || strings.Contains(err.Error(), "AAWrong") {
		t.Fatalf("a wrong token: %v", err)
	}

	secrets := declaredSecrets(map[string]string{"bot_token": " " + testToken + " "})
	settings, label, err := x.Validate(ctx, fields, secrets)
	if err != nil {
		t.Fatal(err)
	}
	s := settings.(Settings)
	if label != "@rubi_test_bot (bot)" || s.Kind != kindBotAccount || s.ID != 900 || s.Username != "rubi_test_bot" {
		t.Fatalf("bot: %q %+v", label, s)
	}
	if secrets["bot_token"] != testToken || secrets["api_hash"] != "" || secrets["session"] != "" {
		t.Fatalf("stored secrets: %v", secrets)
	}
}

func TestSetupPersonalWithCode(t *testing.T) {
	l := &fakeLogin{code: "12345", self: selfInfo{ID: 42, Username: "me_on_tg", Name: "Me"}}
	logins := useFakeLogin(t, l)
	x := &integration{}
	ctx := context.Background()

	fields := map[string]string{"kind": choiceUser}
	_, _, err := x.Validate(ctx, fields, declaredSecrets(nil))
	more := needMore(t, err)
	for _, k := range []string{"api_id", "phone", "api_hash"} {
		if !hasInput(more, k) {
			t.Fatalf("step 1 doesn't ask for %s: %+v", k, more)
		}
	}
	if !strings.Contains(more.Message, "my.telegram.org") || !strings.Contains(more.Message, "API development tools") {
		t.Fatalf("step 1 should explain where the keys come from: %s", more.Message)
	}

	// Step 2: keys and phone; the code is sent.
	fields[rubiplugin.StepField] = more.Step
	fields["api_id"], fields["phone"] = "1234567", "+44 7700 900-123"
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"api_hash": testAPIHash}))
	more = needMore(t, err)
	if !hasInput(more, "code") || *logins != 1 || l.phone != "+447700900123" {
		t.Fatalf("step 2: %+v logins=%d phone=%q", more, *logins, l.phone)
	}

	// Step 3: a wrong code asks again (secrets of earlier steps aren't resent).
	fields[rubiplugin.StepField] = more.Step
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"code": "99999"}))
	more = needMore(t, err)
	if !strings.Contains(more.Message, "wrong") || !hasInput(more, "code") {
		t.Fatalf("wrong code: %+v", more)
	}

	// The old step token is spent.
	if _, _, err := x.Validate(ctx, fields, declaredSecrets(map[string]string{"code": "12345"})); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("reused step: %v", err)
	}

	fields[rubiplugin.StepField] = more.Step
	secrets := declaredSecrets(map[string]string{"code": "1 2 3 4 5"})
	settings, label, err := x.Validate(ctx, fields, secrets)
	if err != nil {
		t.Fatal(err)
	}
	s := settings.(Settings)
	if label != "@me_on_tg" || s.Kind != kindUserAccount || s.APIID != testAPIID || s.ID != 42 || s.Phone != "+447700900123" {
		t.Fatalf("personal: %q %+v", label, s)
	}
	if secrets["api_hash"] != testAPIHash || secrets["session"] != testSession || secrets["code"] != "" || len(secrets) != 3 {
		t.Fatalf("stored secrets: %v", secrets)
	}
	if !l.isClosed() {
		t.Fatal("the login connection stays open after setup")
	}
}

func TestSetupPersonalWith2FA(t *testing.T) {
	l := &fakeLogin{code: "777", password: "correct horse", self: selfInfo{ID: 43, Name: "No Username"}}
	useFakeLogin(t, l)
	x := &integration{}
	ctx := context.Background()

	fields := map[string]string{"kind": choiceUser}
	_, _, err := x.Validate(ctx, fields, declaredSecrets(nil))
	fields[rubiplugin.StepField] = needMore(t, err).Step
	fields["api_id"], fields["phone"] = "1234567", "+1 555 010 4567"
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"api_hash": testAPIHash}))
	fields[rubiplugin.StepField] = needMore(t, err).Step
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"code": "777"}))
	more := needMore(t, err)
	if !hasInput(more, "password") || !strings.Contains(more.Message, "doesn't keep it") {
		t.Fatalf("2FA step: %+v", more)
	}

	fields[rubiplugin.StepField] = more.Step
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"password": "wrong"}))
	more = needMore(t, err)
	if !strings.Contains(more.Message, "wrong") {
		t.Fatalf("wrong password: %+v", more)
	}

	fields[rubiplugin.StepField] = more.Step
	secrets := declaredSecrets(map[string]string{"password": "correct horse"})
	_, label, err := x.Validate(ctx, fields, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if label != "+…4567" {
		t.Fatalf("label %q", label)
	}
	if _, kept := secrets["password"]; kept || secrets["session"] != testSession || secrets["api_hash"] != testAPIHash {
		t.Fatalf("stored secrets: %v", secrets)
	}
}

func TestSetupStepTimeout(t *testing.T) {
	old := pendingTTL
	pendingTTL = 50 * time.Millisecond
	t.Cleanup(func() { pendingTTL = old })
	l := &fakeLogin{code: "12345", self: selfInfo{ID: 42, Username: "me"}}
	useFakeLogin(t, l)
	x := &integration{}
	ctx := context.Background()

	fields := map[string]string{"kind": choiceUser}
	_, _, err := x.Validate(ctx, fields, declaredSecrets(nil))
	fields[rubiplugin.StepField] = needMore(t, err).Step
	fields["api_id"], fields["phone"] = "1234567", "+447700900123"
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"api_hash": testAPIHash}))
	fields[rubiplugin.StepField] = needMore(t, err).Step

	time.Sleep(150 * time.Millisecond)
	if !l.isClosed() {
		t.Fatal("an abandoned login stays connected")
	}
	_, _, err = x.Validate(ctx, fields, declaredSecrets(map[string]string{"code": "12345"}))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("late code: %v", err)
	}
	x.logins.mu.Lock()
	left := len(x.logins.m)
	x.logins.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d logins left in memory", left)
	}
}
