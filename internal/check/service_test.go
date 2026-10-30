package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway implements a minimal /v1/models + /v1/chat/completions.
type fakeGateway struct {
	mu       sync.Mutex
	answer   string
	status   int
	requests int
	models   []string // models seen in probe bodies
}

func (f *fakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.URL.Path == "/v1/models" {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "prov/a", "owned_by": "prov"}, {"id": "prov/b", "owned_by": "prov"}},
		})
		return
	}
	if r.URL.Path == "/v1/chat/completions" {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.models = append(f.models, body.Model)
		if f.status != 0 {
			http.Error(w, "boom", f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": f.answer},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 42},
		})
		return
	}
	http.NotFound(w, r)
}

func newTestService(t *testing.T, gw *fakeGateway) (*Service, *Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate := filepath.Join(dir, "gate.middleware.js")
	svc, err := NewService(store, gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc, store, gate
}

func TestPublishGateBlocksFailedModel(t *testing.T) {
	gw := &fakeGateway{}
	svc, _, gate := newTestService(t, gw)
	if err := svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, State: StateFailed, Reason: "糖果题未通过"}); err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile(gate)
	if err != nil {
		t.Fatal(err)
	}
	js := string(code)
	// null-body guard: a thrown TypeError in the middleware fails open
	if !strings.Contains(js, `if(!body||typeof body.model!=="string")return body;`) {
		t.Error("gate missing null-body guard")
	}
	// probe alias rewrite present
	// gate rejects blocked models
	if !strings.Contains(js, "ctx.reject(403") {
		t.Error("gate missing reject call")
	}
	if !strings.Contains(js, "prov/a") || !strings.Contains(js, "已禁用") {
		t.Error("gate missing blocked target")
	}
}

func TestProbeRoundEndToEnd(t *testing.T) {
	// passing gateway
	gw := &fakeGateway{answer: "最少需要取出 21 个糖果。"}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()

	svc, store, _ := newTestService(t, gw)
	cfg, _ := store.LoadConfig()
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	if err := svc.UpsertTarget(tgt); err != nil {
		t.Fatal(err)
	}
	res, err := svc.runRound(tstCtx(), tgt, mustConfig(t, store), "manual")
	if err != nil {
		t.Fatalf("runRound: %v", err)
	}
	if res.Verdict != VerdictPassed {
		t.Errorf("verdict = %s (detail %s), want passed", res.Verdict, res.Detail)
	}
	tt, _ := store.Target("prov/a")
	if tt.State != StatePassed {
		t.Errorf("target state = %s, want passed", tt.State)
	}
	// probe must have used the alias prefix
	gw.mu.Lock()
	if len(gw.models) == 0 || !strings.HasPrefix(gw.models[0], ProbeAliasPrefix) || !strings.HasSuffix(gw.models[0], "/prov/a") {
		t.Errorf("probe model = %v, want alias-wrapped", gw.models)
	}
	gw.mu.Unlock()

	// failing gateway → failed state with retry
	gw.mu.Lock()
	gw.answer = "我认为需要取出 35 个糖果。"
	gw.mu.Unlock()
	svc2res, err := svc.runRound(tstCtx(), tt, mustConfig(t, store), "scheduled")
	if err != nil {
		t.Fatalf("runRound2: %v", err)
	}
	if svc2res.Verdict != VerdictFailed {
		t.Errorf("verdict2 = %s (detail %s), want failed", svc2res.Verdict, svc2res.Detail)
	}
	tt2, _ := store.Target("prov/a")
	if tt2.State != StateFailed || tt2.FailStreak != 1 {
		t.Errorf("state after fail = %s streak=%d", tt2.State, tt2.FailStreak)
	}
	if !tt2.NextCheckAt.After(time.Now()) {
		t.Error("failed target must be scheduled for recheck")
	}
}

func TestProbeUnavailableOnGatewayError(t *testing.T) {
	gw := &fakeGateway{status: 500}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	_ = svc.Configure(cfg)
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	_ = svc.UpsertTarget(tgt)
	res, err := svc.runRound(tstCtx(), tgt, mustConfig(t, store), "scheduled")
	if err == nil && res.Verdict == VerdictPassed {
		t.Fatal("500 gateway must not pass")
	}
	tt, _ := store.Target("prov/a")
	if tt.State != StateUnavailable {
		t.Errorf("state = %s, want unavailable", tt.State)
	}
}

func TestSchedulerRunsDueTarget(t *testing.T) {
	gw := &fakeGateway{answer: "最少取 21 个"}
	ts := httptest.NewServer(http.HandlerFunc(gw.handler))
	defer ts.Close()
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = ts.URL
	cfg.GatewayKey = "magpie"
	_ = svc.Configure(cfg)
	// due in the past
	_ = svc.UpsertTarget(&Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}, NextCheckAt: time.Now().Add(-time.Minute)})
	svc.schedule()
	// wait for the async round to finish
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tt, _ := store.Target("prov/a")
		if tt.State == StatePassed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	tt, _ := store.Target("prov/a")
	t.Fatalf("scheduled round never completed; state=%s", tt.State)
}

func TestDailyBudgetExhausted(t *testing.T) {
	gw := &fakeGateway{answer: "21"}
	svc, store, _ := newTestService(t, gw)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = "http://127.0.0.1:1"
	cfg.GatewayKey = "k"
	cfg.DailyLimit = 1
	_ = svc.Configure(cfg)
	tgt := &Target{Model: "prov/a", Enabled: true, PromptIDs: []string{"candy"}}
	_ = svc.UpsertTarget(tgt)
	// consume the single-probe daily budget with an earlier probe record
	_ = store.AppendResult(&Result{ID: "seed", Model: "prov/a", Verdict: VerdictPassed, CreatedAt: time.Now()})
	res, _ := svc.runRound(tstCtx(), tgt, cfg, "scheduled")
	if res == nil || res.Verdict != VerdictUnavailable || !strings.Contains(res.Detail, "预算") {
		t.Fatalf("budget exhaustion should mark round unavailable, got %+v", res)
	}
}

// helpers

func mustConfig(t *testing.T, s *Store) GuardConfig {
	t.Helper()
	c, err := s.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tstCtx() context.Context { return context.Background() }
