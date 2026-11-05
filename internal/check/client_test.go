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
	}, "codex/gpt-5.4", "北冥@example.com", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "21" || tokens != 3 {
		t.Fatalf("answer=%q tokens=%d", answer, tokens)
	}
	if gotModel != "codex/gpt-5.4" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotAccount != "北冥@example.com" {
		t.Fatalf("account header = %q, want raw account name", gotAccount)
	}
	if gotSession != "" {
		t.Fatalf("account probe carried a probe session: %q", gotSession)
	}
}

func TestCallPinnedEscapesLiteralPercentInAccount(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Magpie-Account")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "21"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 3},
		})
	}))
	defer server.Close()
	if _, _, err := CallPinned(context.Background(), ProbeConfig{
		GatewayURL: server.URL, GatewayKey: "probe-key",
	}, "codex/gpt-5.4", "a%2Eb@example.com · Team", "question", ""); err != nil {
		t.Fatal(err)
	}
	if got != "a%252Eb@example.com%20%C2%B7%20Team" {
		t.Fatalf("account header = %q, want escaped literal percent", got)
	}
}

func TestProbeSendsReasoningEffortOnlyWhenSet(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "21"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 3},
		})
	}))
	defer server.Close()
	cfg := ProbeConfig{GatewayURL: server.URL, GatewayKey: "probe-key"}
	if _, _, err := Call(context.Background(), cfg, "prov/a", "q", nil, "high"); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
	if _, _, err := Call(context.Background(), cfg, "prov/a", "q", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("empty effort was sent: %v", got["reasoning_effort"])
	}
}
