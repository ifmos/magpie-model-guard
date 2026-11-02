package check

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestListenBackendPersistsAddress(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ln, addr, err := listenBackend(store, "")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	_ = ln.Close()
	saved, ok, err := store.BackendAddr()
	if err != nil || !ok || saved != addr {
		t.Fatalf("saved=%q ok=%v err=%v, want %q", saved, ok, err, addr)
	}

	ln, addr2, err := listenBackend(store, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("second bind: %v", err)
	}
	defer ln.Close()
	if addr2 != addr {
		t.Fatalf("second bind addr=%q, want persisted %q", addr2, addr)
	}
}

func TestListenBackendSavedPortInUseFails(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err := store.SaveBackendAddr(occupied.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenBackend(store, ""); err == nil || !strings.Contains(err.Error(), occupied.Addr().String()) {
		t.Fatalf("occupied saved port must fail with the saved address, got %v", err)
	}

	// An explicit override restores service and becomes the new saved address.
	replacement := freePort(t)
	ln, addr, err := listenBackend(store, replacement)
	if err != nil {
		t.Fatalf("explicit recovery: %v", err)
	}
	defer ln.Close()
	if addr != replacement {
		t.Fatalf("addr=%q, want %q", addr, replacement)
	}
	saved, _, err := store.BackendAddr()
	if err != nil || saved != replacement {
		t.Fatalf("saved=%q err=%v, want %q", saved, err, replacement)
	}
}

func TestListenBackendUnreadableStateDoesNotOverwrite(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`DROP TABLE kv`); err != nil {
		t.Fatal(err)
	}
	ln, addr, err := listenBackend(store, "127.0.0.1:0")
	if err != nil || addr == "" {
		t.Fatalf("bind with unreadable kv: addr=%q err=%v", addr, err)
	}
	_ = ln.Close()
	if _, _, err := store.BackendAddr(); err == nil {
		t.Fatal("kv must stay unwritable, not recreated")
	}
}

func TestNewServiceKeepsProviderKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	gate := filepath.Join(dir, "gate.js")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := newService(store, gate, false)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.LoadConfig()
	if err != nil || first.ProviderKey == "" {
		t.Fatalf("first key=%q err=%v", first.ProviderKey, err)
	}
	svc.Close()
	store.Close()

	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err = newService(store, gate, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	second, err := store.LoadConfig()
	if err != nil || second.ProviderKey != first.ProviderKey {
		t.Fatalf("second key=%q, want %q (err=%v)", second.ProviderKey, first.ProviderKey, err)
	}
}

func readHandshake(t *testing.T, path string) ResponseModelHook {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	var hook ResponseModelHook
	if err := json.Unmarshal(raw, &hook); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	return hook
}

func ruleServiceWithTarget(t *testing.T) (*Service, *Store, string) {
	t.Helper()
	svc, store := newRuleTestService(t)
	hookPath := filepath.Join(t.TempDir(), "response-hook.json")
	if err := svc.ConfigureResponseHook(hookPath, "http://127.0.0.1:1", "control"); err != nil {
		t.Fatalf("ConfigureResponseHook: %v", err)
	}
	if err := store.SaveTarget(&Target{Model: "vendor/big", Enabled: true, IntervalMin: 30}); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, store)
	cfg.GatewayURL = "http://127.0.0.1:1"
	cfg.GatewayKey = "gw"
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{
		ID: "rule-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}}
	if err := svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return svc, store, hookPath
}

func TestProviderModeOwnsResponseJudgment(t *testing.T) {
	svc, store, hookPath := ruleServiceWithTarget(t)
	cfg := mustConfig(t, store)

	// Async observation is enabled while the provider is off.
	hook := readHandshake(t, hookPath)
	if !hook.Enabled {
		t.Fatal("handshake must be enabled with provider off and rules present")
	}
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/big", ActualModel: "vendor/flash", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	target, _ := store.Target("vendor/big")
	if target == nil || target.State != StateFailed {
		t.Fatalf("async mode must disable the target: %+v", target)
	}
	// Reset the target for the provider-mode assertion.
	target.State = StatePassed
	target.Reason = ""
	if err := store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}

	cfg = mustConfig(t, store)
	cfg.ProviderEnabled = true
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	hook = readHandshake(t, hookPath)
	if hook.Enabled {
		t.Fatal("handshake must be disabled while the provider judges synchronously")
	}
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/big", ActualModel: "vendor/flash", Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	target, _ = store.Target("vendor/big")
	if target == nil || target.State == StateFailed {
		t.Fatalf("a late async event must not act in provider mode: %+v", target)
	}
}

func newAPITestServer(t *testing.T, svc *Service) (*httptest.Server, string) {
	t.Helper()
	access := NewAccess("", "")
	control := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, control
}

func postConfig(t *testing.T, ts *httptest.Server, control, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+control)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sink map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&sink)
	return resp.StatusCode
}

func TestConfigAPIKeepsProviderWhenFlagOmitted(t *testing.T) {
	svc, store := newRuleTestService(t)
	ts, control := newAPITestServer(t, svc)
	base := `"gateway_url":"http://127.0.0.1:1","gateway_key":"gw","daily_limit":10,"timeout_sec":30`

	if status := postConfig(t, ts, control, "/api/config", "{"+base+`,"provider_enabled":true}`); status != http.StatusOK {
		t.Fatalf("enable status=%d", status)
	}
	if cfg := mustConfig(t, store); !cfg.ProviderEnabled {
		t.Fatal("provider must be enabled")
	}
	// A partial save that omits provider_enabled must not turn the provider off.
	if status := postConfig(t, ts, control, "/api/config", "{"+base+"}"); status != http.StatusOK {
		t.Fatalf("partial save status=%d", status)
	}
	if cfg := mustConfig(t, store); !cfg.ProviderEnabled {
		t.Fatal("omitted provider_enabled must keep the saved mode")
	}
	if status := postConfig(t, ts, control, "/api/config", "{"+base+`,"provider_enabled":false}`); status != http.StatusOK {
		t.Fatalf("disable status=%d", status)
	}
	if cfg := mustConfig(t, store); cfg.ProviderEnabled {
		t.Fatal("explicit false must disable the provider")
	}
}

func TestConfigureInitialKeepsProviderWhenFlagOmitted(t *testing.T) {
	svc, store := newRuleTestService(t)
	seed := defaultConfig()
	seed.ProviderEnabled = true
	seed.ProviderKey = "k"
	if err := store.SaveConfig(seed); err != nil {
		t.Fatal(err)
	}
	cfg := GuardConfig{
		GatewayURL: "http://127.0.0.1:1", GatewayKey: "gw", DailyLimit: 10, TimeoutSec: 30,
	}
	if err := svc.ConfigureInitial(cfg, nil); err != nil {
		t.Fatalf("ConfigureInitial: %v", err)
	}
	if got := mustConfig(t, store); !got.ProviderEnabled {
		t.Fatal("initial save with a nil flag must keep the saved provider mode")
	}

	off := false
	if err := svc.ConfigureInitial(GuardConfig{}, &off); err != nil {
		t.Fatalf("ConfigureInitial explicit: %v", err)
	}
	if got := mustConfig(t, store); got.ProviderEnabled {
		t.Fatal("explicit false through ConfigureInitial must apply")
	}
}

func TestHandshakeFailureRestoresConfigAndHealth(t *testing.T) {
	svc, store, hookPath := ruleServiceWithTarget(t)
	// Point the handshake at an uncreatable path so both the new write and the
	// restore fail, which must surface in Health instead of claiming success.
	bad := filepath.Join(t.TempDir(), "missing", "response-hook.json")
	if err := svc.ConfigureResponseHook(bad, "http://127.0.0.1:1", "control"); err == nil {
		t.Fatal("handshake into a missing directory must fail")
	}
	cfg := mustConfig(t, store)
	cfg.ProviderEnabled = true
	if err := svc.Configure(cfg); err == nil || !strings.Contains(err.Error(), "握手") {
		t.Fatalf("mode switch must fail with handshake error, got %v", err)
	}
	if got := mustConfig(t, store); got.ProviderEnabled {
		t.Fatal("failed handshake must restore the saved provider mode")
	}
	if health := svc.Health(); !strings.Contains(health, "配置回滚失败") {
		t.Fatalf("health must record the failed rollback, got %q", health)
	}
	if hook := readHandshake(t, hookPath); !hook.Enabled {
		t.Fatal("previous handshake must be restored")
	}
}

func TestRunFailsWhenHandshakeCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	// A directory at the handshake path makes the atomic rename fail.
	if err := os.Mkdir(filepath.Join(dir, "response-hook.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := Run(Options{
		DataDir:  dir,
		GatePath: filepath.Join(dir, "gate.js"),
		Addr:     "127.0.0.1:0",
	})
	if err == nil || !strings.Contains(err.Error(), "response hook") {
		t.Fatalf("startup must fail when the handshake cannot be written, got %v", err)
	}
}

func TestSnapshotExposesProviderRoot(t *testing.T) {
	svc, _ := newRuleTestService(t)
	svc.mu.Lock()
	svc.providerBase = "http://127.0.0.1:9/provider/v1"
	svc.providerRoot = "http://127.0.0.1:9/provider"
	svc.mu.Unlock()
	snap, err := svc.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap["provider_base"] != "http://127.0.0.1:9/provider/v1" || snap["provider_root"] != "http://127.0.0.1:9/provider" {
		t.Fatalf("snapshot provider addresses = %v / %v", snap["provider_base"], snap["provider_root"])
	}
}

func TestAPIResponseModelSkipsInProviderMode(t *testing.T) {
	svc, store, _ := ruleServiceWithTarget(t)
	cfg := mustConfig(t, store)
	cfg.ProviderEnabled = true
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	ts, control := newAPITestServer(t, svc)
	body := `{"requested_model":"vendor/big","actual_model":"vendor/flash","success":true}`
	if status := postConfig(t, ts, control, "/api/response-model", body); status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	// The handler runs asynchronously when it does act; give it a chance and
	// confirm the target stayed usable.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		target, _ := store.Target("vendor/big")
		if target != nil && target.State == StateFailed {
			t.Fatal("async event must be dropped in provider mode")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
