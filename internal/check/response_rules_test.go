package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponseModelRuleMatching(t *testing.T) {
	rules := []ResponseModelRule{
		{ID: "exact", Match: ResponseMatchExact, Pattern: "Vendor/Model", Action: ResponseActionDisable},
		{ID: "glob", Match: ResponseMatchGlob, Pattern: "vendor/*-flash", Action: ResponseActionRecord},
		{ID: "regex", Match: ResponseMatchRegex, Pattern: `^acme/(mini|nano).*`, Action: ResponseActionReroute},
		{ID: "off", Match: ResponseMatchContains, Pattern: "never", Disabled: true, Action: ResponseActionDisable},
	}
	if hit := MatchResponseRule(rules, "vendor/model"); hit == nil || hit.ID != "exact" {
		t.Fatalf("exact rule not matched: %+v", hit)
	}
	if hit := MatchResponseRule(rules, "vendor/x-flash"); hit == nil || hit.ID != "glob" {
		t.Fatalf("glob rule not matched: %+v", hit)
	}
	if hit := MatchResponseRule(rules, "acme/Mini-1"); hit == nil || hit.ID != "regex" {
		t.Fatalf("regex rule not matched: %+v", hit)
	}
	if hit := MatchResponseRule(rules, "never/matches"); hit != nil {
		t.Fatalf("disabled rule matched")
	}
}

func TestResponseModelRuleValidation(t *testing.T) {
	valid := []ResponseModelRule{{
		Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}}
	out, err := NormalizeResponseRules(valid)
	if err != nil || len(out) != 1 || out[0].ID == "" {
		t.Fatalf("valid rules rejected: %+v %v", out, err)
	}
	bad := []ResponseModelRule{{
		Match: ResponseMatchRegex, Pattern: "(", Action: ResponseActionDisable,
	}}
	if _, err := NormalizeResponseRules(bad); err == nil {
		t.Fatalf("invalid regex accepted")
	}
	retry := []ResponseModelRule{{
		Match: ResponseMatchContains, Pattern: "flash", Action: "retry",
	}}
	if _, err := NormalizeResponseRules(retry); err == nil {
		t.Fatalf("retry action must be rejected")
	}
}

func TestEffectiveResponseRules(t *testing.T) {
	global := []ResponseModelRule{{ID: "g", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord}}
	custom := []ResponseModelRule{{ID: "c", Match: ResponseMatchContains, Pattern: "mini", Action: ResponseActionRecord}}
	cfg := GuardConfig{ResponseModelMode: ResponseModelOff, ResponseRules: global}
	if rules := effectiveResponseRules(cfg, &Target{ResponseRuleMode: ResponseRuleInherit}); len(rules) != 0 {
		t.Fatalf("global rules must be off: %+v", rules)
	}
	cfg.ResponseModelMode = ResponseModelOn
	if rules := effectiveResponseRules(cfg, &Target{ResponseRuleMode: ResponseRuleInherit}); len(rules) != 1 || rules[0].ID != "g" {
		t.Fatalf("expected global rules: %+v", rules)
	}
	if rules := effectiveResponseRules(cfg, &Target{ResponseRuleMode: ResponseRuleCustom, ResponseModelRules: custom}); len(rules) != 1 || rules[0].ID != "c" {
		t.Fatalf("expected custom override: %+v", rules)
	}
}

func newRuleTestService(t *testing.T) (*Service, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	svc, err := newService(store, filepath.Join(t.TempDir(), "gate.js"), false)
	if err != nil {
		store.Close()
		t.Fatalf("newService: %v", err)
	}
	t.Cleanup(func() { svc.Close(); store.Close() })
	return svc, store
}

func modelReplyServer(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	var n atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		model := models[len(models)-1]
		if i < len(models) {
			model = models[i]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": model,
			"choices": []any{map[string]any{
				"message":       map[string]string{"content": "21"},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"total_tokens": 3},
		})
	}))
}

func TestProbeDoesNotApplyResponseRules(t *testing.T) {
	svc, store := newRuleTestService(t)
	srv := modelReplyServer(t, "vendor/flash")
	defer srv.Close()

	target := &Target{
		Model: "vendor/big", Enabled: true, IntervalMin: 30,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r1", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}},
	}
	if err := store.SaveTarget(target); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}
	p := &Prompt{ID: "p", Name: "p", Text: "q", Grader: "number", Expect: "21", Automatic: true}
	cfg := GuardConfig{GatewayURL: srv.URL, GatewayKey: "k", DailyLimit: 10, TimeoutSec: 10}
	res, err := svc.probeOnce(context.Background(), target, p, cfg, "test")
	if err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
	if res.Verdict != VerdictPassed || res.RuleID != "" || res.ActualModel != "" {
		t.Fatalf("probe must only grade the answer: %+v", res)
	}
}

func TestHandleResponseModelDisablesAndReroutes(t *testing.T) {
	svc, store := newRuleTestService(t)
	target := &Target{
		Model: "vendor/big", Enabled: true, IntervalMin: 30,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{
			{ID: "r-disable", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable},
			{ID: "r-reroute", Match: ResponseMatchContains, Pattern: "mini", Action: ResponseActionReroute},
		},
	}
	if err := store.SaveTarget(target); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}

	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/big", ActualModel: "vendor/flash", Success: true,
	}); err != nil {
		t.Fatalf("HandleResponseModel disable: %v", err)
	}
	got, _ := store.Target("vendor/big")
	if got == nil || got.State != StateFailed || !strings.Contains(got.Reason, "flash") {
		t.Fatalf("target not disabled: %+v", got)
	}

	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/big", ActualModel: "vendor/mini", Success: true,
	}); err != nil {
		t.Fatalf("HandleResponseModel reroute: %v", err)
	}
	routes, err := store.ResponseReroutes(time.Now().UTC())
	if err != nil || len(routes) != 1 || routes[0] != "vendor/mini" {
		t.Fatalf("expected reroute, got %v err=%v", routes, err)
	}
	res, err := store.Results("vendor/big", "response-model", 10)
	if err != nil || len(res) != 2 {
		t.Fatalf("expected two response results, got %d err=%v", len(res), err)
	}
	for _, r := range res {
		if r.Source != "response" || r.ActualModel == "" {
			t.Fatalf("bad response result: %+v", r)
		}
	}
}

func TestStoreResponseReroutesPrune(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.AddResponseReroute("vendor/old", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddResponseReroute("vendor/new", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	routes, err := store.ResponseReroutes(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != "vendor/new" {
		t.Fatalf("expected only unexpired reroute, got %v", routes)
	}
}

func TestTargetResponseRulesPersist(t *testing.T) {
	svc, store := newRuleTestService(t)
	target := &Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r1", Match: ResponseMatchGlob, Pattern: "vendor/*-flash",
			Action: ResponseActionReroute,
		}},
	}
	if err := svc.UpsertTarget(target); err != nil {
		t.Fatalf("UpsertTarget: %v", err)
	}
	got, err := store.Target("vendor/big")
	if err != nil || got == nil {
		t.Fatalf("Target: %+v %v", got, err)
	}
	if got.ResponseRuleMode != ResponseRuleCustom || len(got.ResponseModelRules) != 1 || got.ResponseModelRules[0].ID != "r1" {
		t.Fatalf("rules not persisted: %+v", got)
	}

	// Updating a target without rule fields must preserve them.
	if err := svc.UpsertTarget(&Target{Model: "vendor/big", Enabled: true}); err != nil {
		t.Fatalf("UpsertTarget preserve: %v", err)
	}
	got, _ = store.Target("vendor/big")
	if got.ResponseRuleMode != ResponseRuleCustom || len(got.ResponseModelRules) != 1 {
		t.Fatalf("rules not preserved: %+v", got)
	}

	// Explicit inherit clears the override.
	if err := svc.UpsertTarget(&Target{Model: "vendor/big", Enabled: true, ResponseRuleMode: ResponseRuleInherit}); err != nil {
		t.Fatalf("UpsertTarget inherit: %v", err)
	}
	got, _ = store.Target("vendor/big")
	if got.ResponseRuleMode != ResponseRuleInherit || len(got.ResponseModelRules) != 0 {
		t.Fatalf("inherit did not clear rules: %+v", got)
	}
}

func TestConfigureResponseRules(t *testing.T) {
	svc, store := newRuleTestService(t)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = "http://127.0.0.1:1"
	cfg.GatewayKey = "magpie"
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{
		Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}}
	if err := svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	got, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.ResponseModelMode != ResponseModelOn || len(got.ResponseRules) != 1 {
		t.Fatalf("config rules not saved: %+v", got)
	}
	if got.ResponseRules[0].ID == "" {
		t.Fatalf("rule id not normalized")
	}
}

func TestResponseModelAPI(t *testing.T) {
	svc, store := newRuleTestService(t)
	if err := store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "api-rule", Match: ResponseMatchContains, Pattern: "flash",
			Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	access := NewAccess("", "")
	token := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)

	body := strings.NewReader(`{"requested_model":"vendor/big","actual_model":"vendor/flash","success":true}`)
	req := httptest.NewRequest("POST", "/api/response-model", body)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoint = %d: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, _ := store.Target("vendor/big")
		if got != nil && got.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("target was not disabled: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHasResponseRules(t *testing.T) {
	response := []ResponseModelRule{{Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord}}
	if hasResponseRules(GuardConfig{ResponseModelMode: ResponseModelOff, ResponseRules: response}, nil) {
		t.Fatalf("global rules must be off when the mode is off")
	}
	if !hasResponseRules(GuardConfig{ResponseModelMode: ResponseModelOn, ResponseRules: response}, nil) {
		t.Fatalf("response rules must enable the business hook")
	}
	targets := []*Target{{Model: "vendor/big", ResponseRuleMode: ResponseRuleCustom, ResponseModelRules: response}}
	if !hasResponseRules(GuardConfig{ResponseModelMode: ResponseModelOff}, targets) {
		t.Fatalf("custom target rules must enable the business hook")
	}
	targets[0].ResponseModelRules[0].Disabled = true
	if hasResponseRules(GuardConfig{ResponseModelMode: ResponseModelOff}, targets) {
		t.Fatalf("disabled rules must not enable the business hook")
	}
}
