package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallPinnedSendsAccountHeaderWithoutAliasSession(t *testing.T) {
	var gotAccount, gotSession, gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccount = r.Header.Get("X-Magpie-Account")
		gotSession = r.Header.Get("X-Magpie-Session")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "21"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 3},
		})
	}))
	defer server.Close()

	answer, tokens, err := CallPinned(context.Background(), ProbeConfig{
		GatewayURL: server.URL, GatewayKey: "probe-key",
	}, "codex/gpt-5.4", "北冥@example.com", "question")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "21" || tokens != 3 {
		t.Fatalf("answer=%q tokens=%d", answer, tokens)
	}
	if gotModel != "codex/gpt-5.4" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotAccount != "%E5%8C%97%E5%86%A5@example.com" {
		t.Fatalf("account header = %q, want percent-encoded", gotAccount)
	}
	if gotSession != "" {
		t.Fatalf("account probe carried a probe session: %q", gotSession)
	}
}
