package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// TestServeSetupOverJSONRPC runs the real plugin over the SDK's protocol, as Rubi does: a bot setup in
// two validate calls, the first answered with need_more.
func TestServeSetupOverJSONRPC(t *testing.T) {
	startBotAPI(t)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := newPlugin(&integration{})
	go func() { _ = p.Serve(context.Background(), inR, outW) }()
	t.Cleanup(func() { inW.Close() })
	rd := bufio.NewScanner(outR)
	call := func(id int, method string, params any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := inW.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		if !rd.Scan() {
			t.Fatal("no answer")
		}
		var res map[string]any
		_ = json.Unmarshal(rd.Bytes(), &res)
		if res["error"] != nil {
			t.Fatalf("%s: %v", method, res["error"])
		}
		return res["result"].(map[string]any)
	}
	call(1, "initialize", rubiplugin.InitializeParams{API: rubiplugin.API, PluginID: "telegram"})
	res := call(2, "validate", rubiplugin.ValidateParams{Fields: map[string]string{"kind": "bot"}, Secrets: declaredSecrets(nil)})
	more, ok := res["need_more"].(map[string]any)
	if !ok || more["step"] == "" {
		t.Fatalf("validate 1: %v", res)
	}
	res = call(3, "validate", rubiplugin.ValidateParams{
		Fields:  map[string]string{"kind": "bot", rubiplugin.StepField: more["step"].(string)},
		Secrets: declaredSecrets(map[string]string{"bot_token": testToken})})
	if res["account"] != "@rubi_test_bot (bot)" || res["account_id"] != "bot:900" || res["secrets"].(map[string]any)["bot_token"] != testToken {
		t.Fatalf("validate 2: %v", res)
	}
}
