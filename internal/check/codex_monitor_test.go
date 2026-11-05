package check

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type codexFakeGateway struct {
	mu         sync.Mutex
	answer     string
	status     int
	account    string
	model      string
	effort     string
	requests   int
	onRequest  func() error
	requestErr error
}

func (g *codexFakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	if g.onRequest != nil {
		g.requestErr = g.onRequest()
	}
	g.account = r.Header.Get("X-Magpie-Account")
	var body struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.model, g.effort = body.Model, body.ReasoningEffort
	if g.status != 0 {
		http.Error(w, "boom", g.status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]string{"content": g.answer},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"total_tokens": 7},
	})
}

func newCodexMonitorTest(t *testing.T) (*Service, *Store, *codexAccountController, *codexFakeGateway, string) {
	t.Helper()
	gw := &codexFakeGateway{answer: "最少需要取出 21 个糖果。"}
	server := httptest.NewServer(http.HandlerFunc(gw.handler))
	t.Cleanup(server.Close)
	svc, store, _ := newTestService(t, nil)
	configDir := t.TempDir()
	mustWriteJSONFile(t, filepath.Join(configDir, "logins.json"), []map[string]any{
		{"id": "aaaaaaaaaaaaaaaa", "agent": "codex", "user": "a@example.com", "plan": "plus"},
		{"id": "bbbbbbbbbbbbbbbb", "agent": "codex", "user": "b@example.com", "plan": "pro"},
	})
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL = server.URL
	cfg.GatewayKey = "probe-secret"
	cfg.TimingPolicy = TimingPolicy{NormalMin: 30, ErrorRetryMin: 2, IsolationFirstMin: 360, IsolationLaterMin: 1440, RecoveryConfirmMin: 2, UnavailableMin: 30}
	cfg.ProbeEffort = "high"
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ctrl := newCodexAccountController(svc, store, configDir)
	svc.codex = ctrl
	if err := store.SaveCodexAccountTarget(&CodexAccountTarget{
		AccountID: "aaaaaaaaaaaaaaaa", User: "a@example.com", Model: "codex/gpt-5.4",
		Enabled: true, State: CodexStateUnknown, NextCheckAt: time.Time{},
	}); err != nil {
		t.Fatal(err)
	}
	return svc, store, ctrl, gw, configDir
}

func mustWriteJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAccountTwoFailsIsolateAndOnePassRestores(t *testing.T) {
	svc, store, ctrl, gw, configDir := newCodexMonitorTest(t)
	ctx := context.Background()
	gw.mu.Lock()
	gw.answer = "我认为需要取出 35 个糖果。"
	gw.mu.Unlock()
	if err := ctrl.runProbe(ctx, "aaaaaaaaaaaaaaaa", true); err != nil {
		t.Fatal(err)
	}
	target, _ := store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if target.Isolated || target.State != CodexStateSuspect || target.FailStreak != 1 {
		t.Fatalf("first failure target = %+v", target)
	}
	if err := ctrl.runProbe(ctx, "aaaaaaaaaaaaaaaa", true); err != nil {
		t.Fatal(err)
	}
	target, _ = store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if !target.Isolated || target.State != CodexStateIsolated || target.FailStreak != 2 {
		t.Fatalf("second failure target = %+v", target)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !target.Isolated || !states["aaaaaaaaaaaaaaaa"].Paused || states["aaaaaaaaaaaaaaaa"].On {
		t.Fatalf("account not disabled: %+v %+v", target, states)
	}
	if !states["bbbbbbbbbbbbbbbb"].On || states["bbbbbbbbbbbbbbbb"].Paused {
		t.Fatalf("fallback account not enabled: %+v", states)
	}
	gw.mu.Lock()
	gotAccount, gotModel, gotEffort := gw.account, gw.model, gw.effort
	gw.answer = "最少需要取出 21 个糖果。"
	gw.mu.Unlock()
	if gotAccount != "a@example.com" || gotModel != "codex/gpt-5.4" {
		t.Fatalf("probe pin/model = %q/%q", gotAccount, gotModel)
	}
	if gotEffort != "high" {
		t.Fatalf("account probe effort = %q, want the global high", gotEffort)
	}
	gw.mu.Lock()
	gw.onRequest = func() error {
		states, err := readCodexLoginStates(configDir)
		if err != nil {
			return err
		}
		if !states["aaaaaaaaaaaaaaaa"].On || states["aaaaaaaaaaaaaaaa"].Paused {
			return errors.New("retest did not temporarily enable the isolated account")
		}
		return nil
	}
	gw.mu.Unlock()
	if err := ctrl.runProbe(ctx, "aaaaaaaaaaaaaaaa", true); err != nil {
		t.Fatal(err)
	}
	gw.mu.Lock()
	if gw.requestErr != nil {
		t.Fatal(gw.requestErr)
	}
	gw.mu.Unlock()
	target, _ = store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if target.Isolated || target.State != CodexStatePassed || target.FailStreak != 0 {
		t.Fatalf("recovery target = %+v", target)
	}
	states, err = readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !states["aaaaaaaaaaaaaaaa"].On || states["aaaaaaaaaaaaaaaa"].Paused {
		t.Fatalf("account not restored: %+v", states)
	}
	if states["bbbbbbbbbbbbbbbb"].On || states["bbbbbbbbbbbbbbbb"].Paused {
		t.Fatalf("fallback not restored: %+v", states)
	}
	if svc.codex == nil {
		t.Fatal("controller lost")
	}
}

func TestCodexAccountUnavailableDoesNotIsolate(t *testing.T) {
	_, store, ctrl, gw, configDir := newCodexMonitorTest(t)
	gw.mu.Lock()
	gw.status = http.StatusInternalServerError
	gw.mu.Unlock()
	if err := ctrl.runProbe(context.Background(), "aaaaaaaaaaaaaaaa", true); err != nil {
		t.Fatal(err)
	}
	target, _ := store.CodexAccountTarget("aaaaaaaaaaaaaaaa")
	if target.Isolated || target.State != CodexStateUnavailable || target.FailStreak != 0 {
		t.Fatalf("unavailable target = %+v", target)
	}
	states, err := readCodexLoginStates(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if states["aaaaaaaaaaaaaaaa"].On || states["aaaaaaaaaaaaaaaa"].Paused || states["bbbbbbbbbbbbbbbb"].On {
		t.Fatalf("unavailable changed accounts: %+v", states)
	}
}
