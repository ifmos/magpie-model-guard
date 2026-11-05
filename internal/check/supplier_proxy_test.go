package check

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"time"
)

func TestSupplierRetriesKeepVendorCredentialAndWireBody(t *testing.T) {
	svc, store := newRuleTestService(t)
	dir := t.TempDir()
	var calls atomic.Int32
	var bodies [][]byte
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if r.URL.Path != "/native/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer selected-vendor-key" || r.Header.Get("X-Vendor-Field") != "kept" {
			t.Error("vendor request changed")
		}
		if r.Header.Get("Cookie") != "vendor-session=selected" {
			t.Error("vendor cookie authentication lost")
		}
		if r.Header.Get("X-Magpie-Response-Model") != "vendor" {
			t.Error("remote vendor model naming lost")
		}
		if r.Header.Get("X-Model-Guard-Session") != "" {
			t.Error("internal session leaked")
		}
		model := "good"
		if n == 1 {
			model = "bad"
		}
		jsonOK(w, `{"model":"`+model+`","choices":[]}`)
	}))
	defer vendor.Close()
	raw := `{"providers":[{"id":"vendor","name":"Vendor","chat":"` + vendor.URL + `/native/v1","key":"never-copy-this","models":["alias"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"modelIDs":{"vendor/alias":"local"},"modelWires":{"vendor/*":"wire-*"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	guard := httptest.NewServer(registerGuardRoutes(http.NewServeMux(), svc))
	defer guard.Close()
	svc.suppliers = newSupplierController(store, dir, guard.URL)
	if err := svc.suppliers.setProtection(context.Background(), "vendor", true); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, store)
	cfg.GatewayURL = vendor.URL
	cfg.GatewayKey = "gateway-key"
	cfg.ProviderAttempts = 2
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{ID: "disable", Match: ResponseMatchExact, Pattern: "bad", Action: ResponseActionDisable}}
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	target := &Target{Model: "vendor/alias", Enabled: true, State: StatePassed, NextCheckAt: time.Now().Add(time.Hour)}
	if err := store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Providers []struct {
			Chat string `json:"chat"`
		} `json:"providers"`
	}
	managed, _ := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err := json.Unmarshal(managed, &config); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"wire-local","messages":[],"unknown":{"preserve":1}}`)
	req, _ := http.NewRequest("POST", config.Providers[0].Chat+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer selected-vendor-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vendor-Field", "kept")
	req.Header.Set("Cookie", "vendor-session=selected")
	req.Header.Set("X-Magpie-Response-Model", "vendor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != `{"model":"good","choices":[]}` || calls.Load() != 2 {
		t.Fatalf("status=%d body=%s calls=%d", resp.StatusCode, got, calls.Load())
	}
	for _, got := range bodies {
		if !bytes.Equal(got, body) {
			t.Fatal("wire body changed across retry")
		}
	}
	latest, err := store.Target("vendor/alias")
	if err != nil || latest.State != StatePassed {
		t.Fatal("successful retry disabled target")
	}
	logged, _, err := store.SupplierEvents(20, "")
	if err != nil {
		t.Fatal(err)
	}
	var sawProtect, sawRetry bool
	for _, ev := range logged {
		if ev.Kind == "protect" && ev.ProviderID == "vendor" {
			sawProtect = true
			if ev.CreatedAt.IsZero() {
				t.Fatal("protect event has no timestamp")
			}
		}
		if ev.Kind == "retry" && ev.ProviderID == "vendor" && ev.ActualModel == "bad" && ev.Model == "vendor/alias" {
			sawRetry = true
		}
	}
	if !sawProtect || !sawRetry {
		t.Fatalf("protection log missing entries: %#v", logged)
	}
	// Live blocked state stops the next business call before sending vendor data.
	latest.State = StateFailed
	if err := store.SaveTarget(latest); err != nil {
		t.Fatal(err)
	}
	blocked, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(body))
	blocked.Header = req.Header.Clone()
	resp, err = http.DefaultClient.Do(blocked)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 || calls.Load() != 2 {
		t.Fatal("blocked target reached vendor")
	}
	// The signed session alias grants only probes the recovery path.
	alias, err := store.Alias()
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := http.NewRequest("POST", req.URL.String(), bytes.NewReader(body))
	probe.Header = req.Header.Clone()
	probe.Header.Set("X-Model-Guard-Session", probeSession(ProbeAliasPrefix+alias+"/vendor/alias"))
	resp, err = http.DefaultClient.Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 3 {
		t.Fatal("recovery probe blocked")
	}
	if err := svc.suppliers.setProtection(context.Background(), "vendor", false); err != nil {
		t.Fatal(err)
	}
	stale, _ := http.NewRequest("POST", req.URL.String(), strings.NewReader(string(body)))
	stale.Header = req.Header.Clone()
	resp, err = http.DefaultClient.Do(stale)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	logged, _, err = store.SupplierEvents(20, "")
	if err != nil {
		t.Fatal(err)
	}
	var sawRestore bool
	for _, ev := range logged {
		if ev.Kind == "restore" && ev.ProviderID == "vendor" {
			sawRestore = true
		}
	}
	if !sawRestore {
		t.Fatalf("restore was not logged: %#v", logged)
	}
	if resp.StatusCode != 404 || calls.Load() != 3 {
		t.Fatal("stale takeover URL remained usable")
	}
}

func TestSupplierRerouteUsesSelectedInternalModel(t *testing.T) {
	svc, store := newRuleTestService(t)
	cfg := mustConfig(t, store)
	rule := ResponseModelRule{ID: "route", Match: ResponseMatchExact, Pattern: "vendor-bad", Action: ResponseActionReroute}
	svc.mu.Lock()
	_, err := svc.applyResponseRuleForRouteLocked(cfg, "relay", "relay/alias", "vendor-bad", "relay/alias", nil, rule, time.Now())
	svc.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	routes, err := store.ResponseReroutes(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != "relay/alias" {
		t.Fatalf("reroutes=%v", routes)
	}
	events := supplierEvents(t, store)
	if len(events) != 1 || events[0].Kind != "action" || events[0].ActualModel != "vendor-bad" ||
		events[0].Action != ResponseActionReroute || events[0].Model != "relay/alias" {
		t.Fatalf("protection log lost reroute: %#v", events)
	}
	if results, _, err := svc.Results("relay/alias", "", 10, ""); err != nil || len(results) != 0 {
		t.Fatalf("rule hit leaked into probe history: %v err=%v", results, err)
	}
}

func TestSupplierSocketJudgesWithProviderDisabled(t *testing.T) {
	svc, store := newRuleTestService(t)
	dir := t.TempDir()
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vendor-key" || r.URL.Path != "/native/v1/responses" {
			t.Error("socket vendor credentials or path changed")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for turn := 0; turn < 2; turn++ {
			_, _, err := c.Read(r.Context())
			if err != nil {
				return
			}
			model := "bad"
			if turn == 1 {
				model = "good"
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"`+model+`","status":"completed","output":[]}}`))
		}
		_, _, _ = c.Read(r.Context())
	}))
	defer vendor.Close()
	raw := `{"providers":[{"id":"vendor","responses":"` + vendor.URL + `/native/v1","models":["big"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	guard := httptest.NewServer(registerGuardRoutes(http.NewServeMux(), svc))
	defer guard.Close()
	svc.suppliers = newSupplierController(store, dir, guard.URL)
	if err := svc.suppliers.setProtection(context.Background(), "vendor", true); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, store)
	cfg.GatewayURL = vendor.URL
	cfg.GatewayKey = "gateway-key"
	cfg.ProviderAttempts = 2
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTarget(&Target{Model: "vendor/big", Enabled: true, ResponseRuleMode: ResponseRuleCustom, ResponseModelRules: []ResponseModelRule{{ID: "disable", Match: ResponseMatchExact, Pattern: "bad", Action: ResponseActionDisable}}}); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Providers []struct {
			Responses string `json:"responses"`
		} `json:"providers"`
	}
	managed, _ := os.ReadFile(filepath.Join(dir, "providers.json"))
	if err := json.Unmarshal(managed, &config); err != nil {
		t.Fatal(err)
	}
	endpoint := "ws" + strings.TrimPrefix(config.Providers[0].Responses, "http") + "/responses"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer vendor-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	writeSocketJSON(t, c, `{"type":"response.create","model":"big","input":[]}`)
	got := readSocketMessage(t, c, 3*time.Second)
	if !strings.Contains(string(got), `"model_guard_blocked"`) || strings.Contains(string(got), `"model":"bad"`) {
		t.Fatalf("blocked socket reply leaked: %s", got)
	}
	latest, err := store.Target("vendor/big")
	if err != nil || latest == nil || !latest.Blocked || latest.BlockSource != BlockSourceRule {
		t.Fatalf("supplier socket did not block selected target: %+v err=%v", latest, err)
	}
}

func TestRemovedDirectProviderEndpointReturns404(t *testing.T) {
	svc, _ := newRuleTestService(t)
	guard := httptest.NewServer(registerGuardRoutes(http.NewServeMux(), svc))
	defer guard.Close()
	for _, path := range []string{"/provider", "/provider/v1/models", "/provider/v1/chat/completions"} {
		resp, err := http.Get(guard.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, resp.StatusCode)
		}
	}
}

func TestSupplierModelExactWirePatternUsesLocalID(t *testing.T) {
	svc, store := newRuleTestService(t)
	dir := t.TempDir()
	svc.suppliers = newSupplierController(store, dir, "http://127.0.0.1:1")
	if err := store.SaveTarget(&Target{Model: "vendor/alias", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"modelIDs":{"vendor/alias":" own "},"modelWires":{"vendor/alias":" wire-* ","vendor/*":"all-*"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	model, target, err := svc.supplierModel("vendor", "wire-alias")
	if err != nil || target == nil || model != "vendor/alias" {
		t.Fatalf("mapped=%s target=%v err=%v", model, target, err)
	}
	model, target, err = svc.supplierModel("vendor", "wire-own")
	if err != nil || target != nil {
		t.Fatalf("exact pattern incorrectly used own id: %s %v %v", model, target, err)
	}
}

// Azure sends its credential only in api-key. The per-key proxy must follow
// that key, or protection silently changes the vendor egress route.
func TestSupplierTransportUsesPreparedAPIKeyForPerKeyProxy(t *testing.T) {
	key := "azure-secret"
	sum := sha256.Sum256([]byte(key))
	binding := supplierBinding{
		Proxy:          "http://provider-proxy.invalid:1",
		AccountProxies: map[string]string{hex.EncodeToString(sum[:5]): "direct"},
	}
	req := httptest.NewRequest("POST", "http://guard.invalid/upstream/token/chat/v1/chat/completions", nil)
	req.Header.Set("Api-Key", key)
	tr, err := supplierTransport(binding, req, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil {
		t.Fatal("per-key direct proxy was not selected from Api-Key")
	}
	if got := supplierCredentialKey(req); got != key {
		t.Fatalf("credential key = %q", got)
	}
}

func TestSupplierEventsAPIListsProtectionLog(t *testing.T) {
	svc, store := newRuleTestService(t)
	for _, ev := range []*SupplierEvent{
		{Kind: "protect", ProviderID: "vendor", Detail: "已启用供应商保护"},
		{Kind: "retry", ProviderID: "vendor", Model: "vendor/big", ActualModel: "vendor/flash", Action: "disable", Detail: "命中规则，已重试"},
	} {
		if err := store.AppendSupplierEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	access := NewAccess("", "")
	token := access.SetControl()
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/supplier-events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated log = %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/supplier-events?limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("log endpoint = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []SupplierEvent `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	events := page.Items
	if len(events) != 2 || events[0].Kind != "retry" || events[1].Kind != "protect" {
		t.Fatalf("log order = %#v", events)
	}
	if events[0].Model != "vendor/big" || events[0].ActualModel != "vendor/flash" || events[0].Action != "disable" {
		t.Fatalf("log fields = %#v", events[0])
	}
}

func TestSupplierEventRetentionCap(t *testing.T) {
	_, store := newRuleTestService(t)
	for i := 0; i < maxSupplierEvents+5; i++ {
		if err := store.AppendSupplierEvent(&SupplierEvent{Kind: "retry", Detail: "event"}); err != nil {
			t.Fatal(err)
		}
	}
	events, _, err := store.SupplierEvents(0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("default limit = %d, want 100", len(events))
	}
	all, _, err := store.SupplierEvents(500, "")
	if err != nil {
		t.Fatal(err)
	}
	// The cap keeps the newest maxSupplierEvents rows: the first inserts are
	// gone, so the oldest row returned is id 6 rather than 1.
	if len(all) != 500 || all[0].ID != int64(maxSupplierEvents+5) || all[len(all)-1].ID != int64(maxSupplierEvents+5-499) {
		t.Fatalf("retention = %d rows, ids %d..%d", len(all), all[0].ID, all[len(all)-1].ID)
	}
}
