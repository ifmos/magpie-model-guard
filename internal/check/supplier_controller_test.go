package check

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const supplierTestOrigin = "http://127.0.0.1:45678"

func newSupplierTestEnv(t *testing.T) (*Store, string, *supplierController) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	configDir := filepath.Join(dir, "magpie")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return store, configDir, newSupplierController(store, configDir, supplierTestOrigin)
}

func supplierTestWriteProviders(t *testing.T, configDir string, data map[string]any) {
	t.Helper()
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("marshal providers: %v", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(configDir, "providers.json"), raw, 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}
}

func supplierTestReadProviders(t *testing.T, configDir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if err != nil {
		t.Fatalf("read providers: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	return data
}

func supplierTestProvider(t *testing.T, data map[string]any, id string) map[string]any {
	t.Helper()
	providers, _ := data["providers"].([]any)
	for _, item := range providers {
		provider, ok := item.(map[string]any)
		if ok && provider["id"] == id {
			return provider
		}
	}
	t.Fatalf("provider %q not found", id)
	return nil
}

func supplierTestRawProvider(t *testing.T, configDir, id string) map[string]json.RawMessage {
	t.Helper()
	_, providers, _, err := supplierReadProvidersFile(filepath.Join(configDir, "providers.json"))
	if err != nil {
		t.Fatalf("read providers: %v", err)
	}
	idx := supplierFindProvider(providers, id)
	if idx < 0 {
		t.Fatalf("provider %q not found", id)
	}
	return providers[idx]
}

func supplierTestRoundTrip(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func supplierTestLedger(t *testing.T, store *Store) supplierLedger {
	t.Helper()
	var ledger supplierLedger
	ok, err := store.getKV(supplierLedgerKey, &ledger)
	if err != nil {
		t.Fatalf("getKV supplier ledger: %v", err)
	}
	if !ok {
		t.Fatal("supplier ledger missing")
	}
	return ledger
}

func supplierTestSetLedger(t *testing.T, store *Store, ledger supplierLedger) {
	t.Helper()
	if err := store.setKV(supplierLedgerKey, ledger); err != nil {
		t.Fatalf("setKV supplier ledger: %v", err)
	}
}

func supplierTestRawLedger(t *testing.T, store *Store) string {
	t.Helper()
	var raw string
	if err := store.db.QueryRow(`SELECT value FROM kv WHERE key=?`, supplierLedgerKey).Scan(&raw); err != nil {
		t.Fatalf("read raw ledger: %v", err)
	}
	return raw
}

func supplierTestEntry(t *testing.T, store *Store, id string) *supplierLedgerEntry {
	t.Helper()
	ledger := supplierTestLedger(t, store)
	entry := ledger.Entries[id]
	if entry == nil {
		t.Fatalf("ledger entry %q missing", id)
	}
	return supplierCloneEntry(entry)
}

func supplierTestBaseProvider() map[string]any {
	return map[string]any{
		"id":        "vendor",
		"name":      "Vendor",
		"key":       "sk-vendor-secret",
		"preset":    "custom",
		"chat":      "https://vendor.example/v1",
		"responses": "https://vendor.example/v1",
		"anthropic": "https://vendor.example/v1/messages",
		"gemini":    "https://gemini.example",
		"decide":    "https://vendor.example/v1",
		"headers": map[string]any{
			"Authorization": "Bearer header-secret",
			"X-Other":       "kept",
		},
		"extraBody": map[string]any{
			"seed":   "seven",
			"nested": map[string]any{"a": []any{"b", "c"}},
		},
		"models":               []any{"vendor/big"},
		"unknownProviderField": map[string]any{"deep": []any{"x", "y"}},
		"proxy":                "socks5://127.0.0.1:1080",
		"accountProxies":       map[string]any{"abcd": "http://proxy.local:8080"},
		"insecureTLS":          true,
	}
}

func supplierTestProvidersFile() map[string]any {
	return map[string]any{
		"providers": []any{
			supplierTestBaseProvider(),
			map[string]any{"id": "sub", "name": "Subscription", "key": "sk-other-secret"},
		},
		"groupOrder": []any{"vendor"},
	}
}

func TestSupplierTakeoverRestorePreservesUnrelatedEditsAndSecrets(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	initial := supplierTestProvidersFile()
	supplierTestWriteProviders(t, configDir, initial)

	before := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	extraBodyBefore := supplierTestRoundTrip(t, before["extraBody"])
	unknownBefore := supplierTestRoundTrip(t, before["unknownProviderField"])

	if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
		t.Fatalf("enable protection: %v", err)
	}
	if active, err := ctrl.active(); err != nil || !active {
		t.Fatalf("active = %v, %v; want true, nil", active, err)
	}

	entry := supplierTestEntry(t, store, "vendor")
	if len(entry.Token) != 48 {
		t.Fatalf("token length = %d, want 48 hex chars", len(entry.Token))
	}
	if entry.Origin != supplierTestOrigin {
		t.Fatalf("origin = %q", entry.Origin)
	}

	provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	wantURLs := map[string]string{
		"chat":      supplierTestOrigin + "/upstream/" + entry.Token + "/chat/v1",
		"responses": supplierTestOrigin + "/upstream/" + entry.Token + "/responses/v1",
		"anthropic": supplierTestOrigin + "/upstream/" + entry.Token + "/anthropic",
		"gemini":    supplierTestOrigin + "/upstream/" + entry.Token + "/gemini/v1beta",
		"decide":    supplierTestOrigin + "/upstream/" + entry.Token + "/decide/v1",
	}
	for protocol, want := range wantURLs {
		if got := provider[protocol]; got != want {
			t.Errorf("%s = %v, want %s", protocol, got, want)
		}
	}
	if provider["proxy"] != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy = %#v, want untouched original", provider["proxy"])
	}
	if !reflect.DeepEqual(supplierTestRoundTrip(t, provider["accountProxies"]), map[string]any{"abcd": "http://proxy.local:8080"}) {
		t.Errorf("accountProxies = %#v, want untouched original", provider["accountProxies"])
	}
	headers, _ := provider["headers"].(map[string]any)
	if headers["X-Model-Guard-Session"] != supplierSessionValue {
		t.Errorf("probe header = %v, want %s", headers["X-Model-Guard-Session"], supplierSessionValue)
	}
	if headers["Authorization"] != "Bearer header-secret" || headers["X-Other"] != "kept" {
		t.Errorf("unrelated headers changed: %#v", headers)
	}
	if provider["key"] != "sk-vendor-secret" {
		t.Errorf("credential changed: %v", provider["key"])
	}
	if !reflect.DeepEqual(supplierTestRoundTrip(t, provider["extraBody"]), extraBodyBefore) {
		t.Errorf("extraBody changed: %#v", provider["extraBody"])
	}
	if !reflect.DeepEqual(supplierTestRoundTrip(t, provider["unknownProviderField"]), unknownBefore) {
		t.Errorf("unknown provider field changed: %#v", provider["unknownProviderField"])
	}

	rawLedger := supplierTestRawLedger(t, store)
	for _, secret := range []string{"sk-vendor-secret", "header-secret", "Bearer ", "extraBody", "unknownProviderField", "socks5://127.0.0.1:1080", "proxy.local"} {
		if strings.Contains(rawLedger, secret) {
			t.Errorf("ledger persisted credential/unrelated field %q: %s", secret, rawLedger)
		}
	}

	list, err := ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !list.Configured || len(list.Providers) != 1 {
		t.Fatalf("list = %#v; want configured with one eligible provider", list)
	}
	info := list.Providers[0]
	if info.ID != "vendor" || info.Name != "Vendor" || !info.Protected || info.Conflict != "" {
		t.Fatalf("provider info = %#v", info)
	}
	wantProtocols := []string{"chat", "responses", "anthropic", "gemini", "decide"}
	if !reflect.DeepEqual(info.Protocols, wantProtocols) {
		t.Fatalf("protocols = %v, want %v", info.Protocols, wantProtocols)
	}

	binding, ok, err := ctrl.binding(entry.Token, "chat")
	if err != nil || !ok {
		t.Fatalf("binding chat = %#v, %v, %v", binding, ok, err)
	}
	if binding.ProviderID != "vendor" || binding.Base != "https://vendor.example/v1" || binding.Protocol != "chat" {
		t.Fatalf("binding = %#v", binding)
	}
	if binding.Proxy != "socks5://127.0.0.1:1080" || !binding.InsecureTLS {
		t.Fatalf("binding proxy/tls = %#v", binding)
	}
	if !reflect.DeepEqual(binding.AccountProxies, map[string]string{"abcd": "http://proxy.local:8080"}) {
		t.Fatalf("binding account proxies = %#v", binding.AccountProxies)
	}
	if binding, ok, err := ctrl.binding(entry.Token, "gemini"); err != nil || !ok || binding.Base != "https://gemini.example" {
		t.Fatalf("binding gemini = %#v, %v, %v", binding, ok, err)
	}
	if _, ok, err := ctrl.binding("000000000000000000000000000000000000000000000000", "chat"); err != nil || ok {
		t.Fatalf("unknown token matched: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ctrl.binding(entry.Token, "video"); err != nil || ok {
		t.Fatalf("unknown protocol matched: ok=%v err=%v", ok, err)
	}
	if protected, err := ctrl.protectedProvider("vendor"); err != nil || !protected {
		t.Fatalf("protectedProvider = %v, %v; want true", protected, err)
	}
	if protected, err := ctrl.protectedProvider("sub"); err != nil || protected {
		t.Fatalf("unprotected provider reported protected: %v, %v", protected, err)
	}

	// A user edits unrelated fields while protection is on. They must survive
	// both list/conflict checks and the eventual restore.
	data := supplierTestReadProviders(t, configDir)
	provider = supplierTestProvider(t, data, "vendor")
	provider["name"] = "Vendor Renamed"
	provider["key"] = "sk-rotated-secret"
	provider["models"] = []any{"vendor/big", "vendor/new"}
	provider["insecureTLS"] = false
	provider["proxy"] = "http://127.0.0.1:18080"
	provider["accountProxies"] = map[string]any{"abcd": "http://changed.local:9090"}
	headers = provider["headers"].(map[string]any)
	headers["X-New"] = "new-value"
	extraBody := provider["extraBody"].(map[string]any)
	extraBody["new"] = true
	unknown := provider["unknownProviderField"].(map[string]any)
	unknown["edited"] = "yes"
	supplierTestWriteProviders(t, configDir, data)

	list, err = ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list after unrelated edits: %v", err)
	}
	if len(list.Providers) != 1 || list.Providers[0].Conflict != "" || !list.Providers[0].Protected {
		t.Fatalf("unrelated edits reported as conflict: %#v", list.Providers)
	}
	if binding, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || !ok || binding.InsecureTLS ||
		binding.Proxy != "http://127.0.0.1:18080" ||
		!reflect.DeepEqual(binding.AccountProxies, map[string]string{"abcd": "http://changed.local:9090"}) {
		t.Fatalf("binding did not use current proxy/TLS fields: %#v, %v, %v", binding, ok, err)
	}

	if err := ctrl.setProtection(ctx, "vendor", false); err != nil {
		t.Fatalf("disable protection: %v", err)
	}
	provider = supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	for protocol, want := range map[string]string{
		"chat":      "https://vendor.example/v1",
		"responses": "https://vendor.example/v1",
		"anthropic": "https://vendor.example/v1/messages",
		"gemini":    "https://gemini.example",
		"decide":    "https://vendor.example/v1",
	} {
		if got := provider[protocol]; got != want {
			t.Errorf("restored %s = %v, want %s", protocol, got, want)
		}
	}
	if provider["proxy"] != "http://127.0.0.1:18080" {
		t.Errorf("proxy edit was reverted: %v", provider["proxy"])
	}
	if !reflect.DeepEqual(supplierTestRoundTrip(t, provider["accountProxies"]), map[string]any{"abcd": "http://changed.local:9090"}) {
		t.Errorf("accountProxies edit was reverted: %#v", provider["accountProxies"])
	}
	headers = provider["headers"].(map[string]any)
	if _, present := headers["X-Model-Guard-Session"]; present {
		t.Errorf("probe header was not restored out: %#v", headers)
	}
	if headers["Authorization"] != "Bearer header-secret" || headers["X-New"] != "new-value" {
		t.Errorf("headers not preserved across restore: %#v", headers)
	}
	if provider["name"] != "Vendor Renamed" || provider["key"] != "sk-rotated-secret" {
		t.Errorf("unrelated edits lost: %#v", provider)
	}
	if models, _ := provider["models"].([]any); len(models) != 2 {
		t.Errorf("models edit lost: %#v", provider["models"])
	}
	if extraBody, _ := provider["extraBody"].(map[string]any); extraBody["new"] != true {
		t.Errorf("extraBody edit lost: %#v", provider["extraBody"])
	}
	if unknown, _ := provider["unknownProviderField"].(map[string]any); unknown["edited"] != "yes" {
		t.Errorf("unknown field edit lost: %#v", provider["unknownProviderField"])
	}
	if active, err := ctrl.active(); err != nil || active {
		t.Fatalf("active after disable = %v, %v", active, err)
	}
	if _, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || ok {
		t.Fatalf("disabled token still bound: ok=%v err=%v", ok, err)
	}
	if ledger := supplierTestLedger(t, store); len(ledger.Entries) != 0 || ledger.Pending != nil {
		t.Fatalf("ledger not cleared: %#v", ledger)
	}
}

func TestHandleResponseModelSkipsUncorrelatedProtectedDirectReplies(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	svc, err := newService(store, filepath.Join(t.TempDir(), "gate.js"), false)
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	t.Cleanup(svc.Close)
	svc.suppliers = ctrl
	supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
	if err := ctrl.setProtection(context.Background(), "vendor", true); err != nil {
		t.Fatalf("enable protection: %v", err)
	}
	cfg := mustConfig(t, store)
	cfg.GatewayURL = "http://127.0.0.1:1"
	cfg.GatewayKey = "gw"
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{
		ID: "record-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord,
	}}
	if err := svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	// A direct provider-scoped event with no reply id falls back to the
	// protected-provider check.
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/model", ActualModel: "vendor/flash", Success: true,
	}); err != nil {
		t.Fatalf("protected fallback: %v", err)
	}
	if results, err := store.Results("vendor/model", "response-model", 10); err != nil || len(results) != 0 {
		t.Fatalf("protected direct event was judged: %d results, err=%v", len(results), err)
	}

	// An unprotected provider keeps async judgment.
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "sub/model", ActualModel: "vendor/flash", Success: true,
	}); err != nil {
		t.Fatalf("unprotected event: %v", err)
	}
	if results, err := store.Results("sub/model", "response-model", 10); err != nil || len(results) != 1 {
		t.Fatalf("unprotected event lost judgment: %d results, err=%v", len(results), err)
	}

	// Magpie can generate an id when the vendor omitted it. A non-empty id
	// unobserved by Guard still belongs to the protected direct request.
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "vendor/other", ActualModel: "vendor/flash",
		ResponseID: "chatcmpl-generated-by-magpie", Success: true,
	}); err != nil {
		t.Fatalf("generated id event: %v", err)
	}
	if results, err := store.Results("vendor/other", "response-model", 10); err != nil || len(results) != 0 {
		t.Fatalf("protected generated id was judged again: %d results, err=%v", len(results), err)
	}
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "sub/other", ActualModel: "vendor/flash",
		ResponseID: "chatcmpl-unprotected", Success: true,
	}); err != nil {
		t.Fatalf("unprotected id event: %v", err)
	}
	if results, err := store.Results("sub/other", "response-model", 10); err != nil || len(results) != 1 {
		t.Fatalf("unprotected id lost judgment: %d results, err=%v", len(results), err)
	}

	// A provider whose id begins "auto-" is a direct provider, not an auto
	// alias; its async judgment must survive while another supplier is active.
	if err := svc.HandleResponseModel(ResponseModelEvent{
		RequestedModel: "auto-relay/model", ActualModel: "vendor/flash",
		ResponseID: "resp_direct_provider", Success: true,
	}); err != nil {
		t.Fatalf("auto-prefixed provider event: %v", err)
	}
	if results, err := store.Results("auto-relay/model", "response-model", 10); err != nil || len(results) != 1 {
		t.Fatalf("auto-prefixed provider lost judgment: %d results, err=%v", len(results), err)
	}

	// A group reply that cannot be correlated with the supplier inspector is
	// skipped while any supplier protection is active: a second action could
	// be destructive and the hook does not name the supplier.
	for _, model := range []string{"group/unseen", "auto/alias"} {
		if err := svc.HandleResponseModel(ResponseModelEvent{
			RequestedModel: model, ActualModel: "vendor/flash",
			ResponseID: "resp_unseen_group", Success: true,
		}); err != nil {
			t.Fatalf("group event %s: %v", model, err)
		}
		if results, err := store.Results(model, "response-model", 10); err != nil || len(results) != 0 {
			t.Fatalf("active group event %s was judged: %d results, err=%v", model, len(results), err)
		}
	}
}

func TestSupplierReconcileMovesOriginAndIsIdempotent(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
	if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	entry := supplierTestEntry(t, store, "vendor")
	beforeBytes, err := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ctrl.reconcile(ctx); err != nil {
		t.Fatalf("reconcile same origin: %v", err)
	}
	afterBytes, _ := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if string(beforeBytes) != string(afterBytes) {
		t.Fatal("reconcile with unchanged origin rewrote providers.json")
	}

	const newOrigin = "http://127.0.0.1:56789"
	newCtrl := newSupplierController(store, configDir, newOrigin)
	if err := newCtrl.reconcile(ctx); err != nil {
		t.Fatalf("reconcile new origin: %v", err)
	}
	provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	if provider["chat"] != newOrigin+"/upstream/"+entry.Token+"/chat/v1" {
		t.Fatalf("chat after origin rebind = %v", provider["chat"])
	}
	if provider["gemini"] != newOrigin+"/upstream/"+entry.Token+"/gemini/v1beta" {
		t.Fatalf("gemini after origin rebind = %v", provider["gemini"])
	}
	rebound := supplierTestEntry(t, store, "vendor")
	if rebound.Token != entry.Token || rebound.Origin != newOrigin {
		t.Fatalf("rebound entry = %#v", rebound)
	}
	reboundBytes, _ := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if err := newCtrl.reconcile(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	againBytes, _ := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if string(reboundBytes) != string(againBytes) {
		t.Fatal("idempotent reconcile rewrote providers.json")
	}
	binding, ok, err := newCtrl.binding(entry.Token, "chat")
	if err != nil || !ok || binding.Base != "https://vendor.example/v1" {
		t.Fatalf("binding after rebind = %#v, %v, %v", binding, ok, err)
	}
	list, err := newCtrl.list(ctx)
	if err != nil || len(list.Providers) != 1 || list.Providers[0].Conflict != "" {
		t.Fatalf("list after rebind = %#v, %v", list, err)
	}
}

func TestSupplierExternalConflictAndProviderDeletion(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
	if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	entry := supplierTestEntry(t, store, "vendor")

	data := supplierTestReadProviders(t, configDir)
	provider := supplierTestProvider(t, data, "vendor")
	provider["chat"] = "https://evil.example/v1"
	supplierTestWriteProviders(t, configDir, data)

	list, err := ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list conflict: %v", err)
	}
	if len(list.Providers) != 1 || !list.Providers[0].Protected || !strings.Contains(list.Providers[0].Conflict, "chat") {
		t.Fatalf("conflict not reported: %#v", list.Providers)
	}
	if err := ctrl.reconcile(ctx); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("reconcile conflict error = %v", err)
	}
	provider = supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	if provider["chat"] != "https://evil.example/v1" {
		t.Fatalf("conflict was overwritten: %v", provider["chat"])
	}
	if err := ctrl.setProtection(ctx, "vendor", false); err == nil {
		t.Fatal("disable overwrote an externally modified managed field")
	}
	provider = supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	if provider["chat"] != "https://evil.example/v1" {
		t.Fatalf("disable wrote despite conflict: %v", provider["chat"])
	}
	if protected, err := ctrl.protectedProvider("vendor"); err != nil || protected {
		t.Fatalf("conflicted provider reported protected: %v, %v", protected, err)
	}

	// Deleting the provider is an explicit release: no Guard URL remains.
	delete(data, "providers")
	data["providers"] = []any{}
	supplierTestWriteProviders(t, configDir, data)
	if err := ctrl.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after provider deletion: %v", err)
	}
	if ledger := supplierTestLedger(t, store); len(ledger.Entries) != 0 || ledger.Pending != nil {
		t.Fatalf("deleted provider left stale ledger: %#v", ledger)
	}
	if active, err := ctrl.active(); err != nil || active {
		t.Fatalf("active after deletion = %v, %v", active, err)
	}
	if _, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || ok {
		t.Fatalf("deleted provider token still bound: ok=%v err=%v", ok, err)
	}
}

func TestSupplierWALRecoveryBeforeAndAfter(t *testing.T) {
	t.Run("write did not land rolls back before", func(t *testing.T) {
		store, configDir, ctrl := newSupplierTestEnv(t)
		ctx := context.Background()
		supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
		if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
			t.Fatalf("enable: %v", err)
		}
		entry := supplierTestEntry(t, store, "vendor")
		beforeBytes, _ := os.ReadFile(filepath.Join(configDir, "providers.json"))

		ledger := supplierTestLedger(t, store)
		ledger.Pending = &supplierPendingTx{
			ProviderID: "vendor",
			Before:     supplierPendingSide{Entry: supplierCloneEntry(entry), Snapshot: supplierCloneSnapshot(entry.After)},
			After:      supplierPendingSide{Snapshot: supplierCloneSnapshot(entry.Original)},
			Timestamp:  time.Now().Unix(),
		}
		supplierTestSetLedger(t, store, ledger)

		if err := ctrl.reconcile(ctx); err != nil {
			t.Fatalf("reconcile rollback: %v", err)
		}
		afterBytes, _ := os.ReadFile(filepath.Join(configDir, "providers.json"))
		if string(beforeBytes) != string(afterBytes) {
			t.Fatal("rollback rewrote providers.json")
		}
		ledger = supplierTestLedger(t, store)
		if ledger.Pending != nil || ledger.Entries["vendor"] == nil {
			t.Fatalf("rollback ledger = %#v", ledger)
		}
		if _, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || !ok {
			t.Fatalf("rollback lost binding: ok=%v err=%v", ok, err)
		}
	})

	t.Run("write landed commits after", func(t *testing.T) {
		store, configDir, ctrl := newSupplierTestEnv(t)
		ctx := context.Background()
		supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
		if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
			t.Fatalf("enable: %v", err)
		}
		entry := supplierTestEntry(t, store, "vendor")
		supplierTestRestoreProviderFile(t, configDir, entry)

		ledger := supplierTestLedger(t, store)
		ledger.Pending = &supplierPendingTx{
			ProviderID: "vendor",
			Before:     supplierPendingSide{Entry: supplierCloneEntry(entry), Snapshot: supplierCloneSnapshot(entry.After)},
			After:      supplierPendingSide{Snapshot: supplierCloneSnapshot(entry.Original)},
			Timestamp:  time.Now().Unix(),
		}
		supplierTestSetLedger(t, store, ledger)

		if err := ctrl.reconcile(ctx); err != nil {
			t.Fatalf("reconcile commit: %v", err)
		}
		ledger = supplierTestLedger(t, store)
		if ledger.Pending != nil || len(ledger.Entries) != 0 {
			t.Fatalf("commit ledger = %#v", ledger)
		}
		provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
		if provider["chat"] != "https://vendor.example/v1" || provider["anthropic"] != "https://vendor.example/v1/messages" {
			t.Fatalf("committed restore wrong: %#v", provider)
		}
		if _, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || ok {
			t.Fatalf("committed disable still bound: ok=%v err=%v", ok, err)
		}
	})

	t.Run("third state keeps pending", func(t *testing.T) {
		store, configDir, ctrl := newSupplierTestEnv(t)
		ctx := context.Background()
		supplierTestWriteProviders(t, configDir, supplierTestProvidersFile())
		if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
			t.Fatalf("enable: %v", err)
		}
		entry := supplierTestEntry(t, store, "vendor")
		ledger := supplierTestLedger(t, store)
		ledger.Pending = &supplierPendingTx{
			ProviderID: "vendor",
			Before:     supplierPendingSide{Entry: supplierCloneEntry(entry), Snapshot: supplierCloneSnapshot(entry.After)},
			After:      supplierPendingSide{Snapshot: supplierCloneSnapshot(entry.Original)},
			Timestamp:  time.Now().Unix(),
		}
		supplierTestSetLedger(t, store, ledger)

		data := supplierTestReadProviders(t, configDir)
		provider := supplierTestProvider(t, data, "vendor")
		provider["chat"] = "https://evil.example/v1"
		supplierTestWriteProviders(t, configDir, data)

		if err := ctrl.reconcile(ctx); err == nil {
			t.Fatal("third state was silently resolved")
		}
		ledger = supplierTestLedger(t, store)
		if ledger.Pending == nil {
			t.Fatal("third state dropped pending recovery information")
		}
		provider = supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
		if provider["chat"] != "https://evil.example/v1" {
			t.Fatalf("third state was overwritten: %v", provider["chat"])
		}
	})
}

func supplierTestRestoreProviderFile(t *testing.T, configDir string, entry *supplierLedgerEntry) {
	t.Helper()
	path := filepath.Join(configDir, "providers.json")
	root, providers, _, err := supplierReadProvidersFile(path)
	if err != nil {
		t.Fatalf("read providers: %v", err)
	}
	idx := supplierFindProvider(providers, entry.ProviderID)
	if idx < 0 {
		t.Fatalf("provider %s missing", entry.ProviderID)
	}
	if err := supplierApplyTransition(providers[idx], entry.After, entry.Original, entry.HeaderState); err != nil {
		t.Fatalf("restore provider fields: %v", err)
	}
	providersRaw, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	root["providers"] = json.RawMessage(providersRaw)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}
}

func TestSupplierValidationLoopsAndEligibility(t *testing.T) {
	_, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()

	data := map[string]any{"providers": []any{
		map[string]any{"id": "sub", "name": "Subscription", "key": "sk-sub"},
		map[string]any{"id": "decide", "name": "Decide", "decide": "https://decide.example/v1"},
	}}
	supplierTestWriteProviders(t, configDir, data)

	list, err := ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Providers) != 1 || list.Providers[0].ID != "decide" || len(list.Providers[0].Protocols) != 1 || list.Providers[0].Protocols[0] != "decide" {
		t.Fatalf("eligibility list = %#v", list.Providers)
	}
	if err := ctrl.setProtection(ctx, "sub", true); err == nil {
		t.Fatal("provider without a custom base was eligible")
	}
	if err := ctrl.setProtection(ctx, "decide", true); err != nil {
		t.Fatalf("decide-only provider: %v", err)
	}
	entry := supplierTestEntry(t, ctrl.store, "decide")
	provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "decide")
	if provider["decide"] != supplierTestOrigin+"/upstream/"+entry.Token+"/decide/v1" {
		t.Fatalf("decide managed URL = %v", provider["decide"])
	}
	if err := ctrl.setProtection(ctx, "decide", false); err != nil {
		t.Fatalf("disable decide: %v", err)
	}

	invalid := []struct {
		name string
		base string
	}{
		{"own-origin", supplierTestOrigin + "/v1"},
		{"upstream-path", "https://127.0.0.1:9000/upstream/abc/v1"},
		{"provider-path", "https://127.0.0.1:9000/provider/v1"},
		{"unsupported-scheme", "ftp://vendor.example/v1"},
		{"userinfo", "https://user:pass@vendor.example/v1"},
		{"query", "https://vendor.example/v1?key=secret"},
		{"fragment", "https://vendor.example/v1#frag"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{"providers": []any{map[string]any{
				"id": "vendor", "name": "Vendor", "chat": tc.base,
			}}}
			supplierTestWriteProviders(t, configDir, data)
			err := ctrl.setProtection(ctx, "vendor", true)
			if err == nil {
				t.Fatalf("base %q accepted", tc.base)
			}
			provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
			if provider["chat"] != tc.base {
				t.Fatalf("rejected base was changed: %v", provider["chat"])
			}
		})
	}

	t.Run("credential-bearing and host-dependent bases are refused", func(t *testing.T) {
		cases := []struct {
			name     string
			provider map[string]any
			conflict string
		}{
			{
				name:     "gemini query key",
				provider: map[string]any{"id": "gemini-key", "gemini": "https://gemini.example/v1beta/models/x:generateContent?key=secret"},
				conflict: "查询参数",
			},
			{
				name:     "azure host",
				provider: map[string]any{"id": "azure-host", "chat": "https://resource.openai.azure.com/openai/v1"},
				conflict: "Azure OpenAI",
			},
			{
				name:     "bedrock host",
				provider: map[string]any{"id": "bedrock-host", "anthropic": "https://bedrock-runtime.us-east-1.amazonaws.com"},
				conflict: "Bedrock",
			},
			{
				name:     "opencode host",
				provider: map[string]any{"id": "opencode-host", "chat": "https://opencode.ai/zen/v1"},
				conflict: "OpenCode",
			},
			{
				name:     "vercel decide",
				provider: map[string]any{"id": "vercel-decide", "decide": "https://ai-gateway.vercel.sh/v4/ai"},
				conflict: "vercel-eval",
			},
			{
				name:     "cloudflare decide",
				provider: map[string]any{"id": "cf-decide", "decide": "https://api.cloudflare.com/client/v4/accounts/x/ai/v1"},
				conflict: "cloudflare",
			},
			{
				name:     "vertex preset",
				provider: map[string]any{"id": "vertex", "preset": "google-vertex", "gemini": "https://us-central1-aiplatform.googleapis.com/v1beta"},
				conflict: "Vertex AI",
			},
			{
				name:     "azure preset",
				provider: map[string]any{"id": "azure", "preset": "azure", "chat": "https://vendor.example/openai/v1"},
				conflict: "Azure OpenAI",
			},
			{
				name: "remote magpie preset",
				provider: map[string]any{
					"id": "remote", "preset": "remote-magpie",
					"chat": "https://other-magpie.example/v1", "responses": "https://other-magpie.example/v1",
					"anthropic": "https://other-magpie.example", "gemini": "https://other-magpie.example/v1beta",
				},
				conflict: "远程 Magpie",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				supplierTestWriteProviders(t, configDir, map[string]any{"providers": []any{tc.provider}})
				id := tc.provider["id"].(string)
				list, err := ctrl.list(context.Background())
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(list.Providers) != 1 || !strings.Contains(list.Providers[0].Conflict, tc.conflict) {
					t.Fatalf("list conflict = %#v", list.Providers)
				}
				if got := strings.Count(list.Providers[0].Conflict, tc.conflict); got != 1 {
					t.Fatalf("conflict %q repeated %d times: %q", tc.conflict, got, list.Providers[0].Conflict)
				}
				if err := ctrl.setProtection(context.Background(), id, true); err == nil || !strings.Contains(err.Error(), tc.conflict) {
					t.Fatalf("setProtection err = %v", err)
				}
				if ledger := supplierTestLedger(t, ctrl.store); len(ledger.Entries) != 0 || ledger.Pending != nil {
					t.Fatalf("refused takeover wrote ledger: %#v", ledger)
				}
				if live := supplierTestProvider(t, supplierTestReadProviders(t, configDir), id); live["chat"] != tc.provider["chat"] || live["gemini"] != tc.provider["gemini"] {
					t.Fatalf("refused takeover changed provider: %#v", live)
				}
			})
		}
	})

	t.Run("non-loopback origin", func(t *testing.T) {
		bad := newSupplierController(ctrl.store, configDir, "http://192.168.1.9:9999")
		data := map[string]any{"providers": []any{map[string]any{
			"id": "vendor", "name": "Vendor", "chat": "https://vendor.example/v1",
		}}}
		supplierTestWriteProviders(t, configDir, data)
		if err := bad.setProtection(ctx, "vendor", true); err == nil {
			t.Fatal("non-loopback origin accepted")
		}
	})

	t.Run("scheme-less bases are validated at runtime and preserved raw", func(t *testing.T) {
		data := map[string]any{"providers": []any{map[string]any{
			"id": "bare", "name": "Bare", "chat": "vendor.example/v1", "gemini": "gemini.example",
		}}}
		supplierTestWriteProviders(t, configDir, data)
		if err := ctrl.setProtection(ctx, "bare", true); err != nil {
			t.Fatalf("scheme-less provider: %v", err)
		}
		entry := supplierTestEntry(t, ctrl.store, "bare")
		binding, ok, err := ctrl.binding(entry.Token, "chat")
		if err != nil || !ok || binding.Base != "vendor.example/v1" {
			t.Fatalf("raw chat base = %#v, %v, %v", binding, ok, err)
		}
		binding, ok, err = ctrl.binding(entry.Token, "gemini")
		if err != nil || !ok || binding.Base != "gemini.example" {
			t.Fatalf("raw gemini base = %#v, %v, %v", binding, ok, err)
		}
		if err := ctrl.setProtection(ctx, "bare", false); err != nil {
			t.Fatalf("disable bare: %v", err)
		}
		provider := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "bare")
		if provider["chat"] != "vendor.example/v1" || provider["gemini"] != "gemini.example" {
			t.Fatalf("raw bases not restored: %#v", provider)
		}
	})
}

func TestSupplierRejectsExistingProbeHeaderCaseInsensitively(t *testing.T) {
	_, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	for _, markerName := range []string{supplierSessionHeader, strings.ToLower(supplierSessionHeader)} {
		t.Run(markerName, func(t *testing.T) {
			provider := map[string]any{
				"id":   "vendor",
				"name": "Vendor",
				"chat": "https://vendor.example/v1",
				"headers": map[string]any{
					"Authorization": "Bearer header-secret",
					markerName:      "user-value",
				},
			}
			supplierTestWriteProviders(t, configDir, map[string]any{"providers": []any{provider}})
			if err := ctrl.setProtection(ctx, "vendor", true); err == nil {
				t.Fatalf("existing header %q was accepted", markerName)
			}
			live := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
			if live["chat"] != "https://vendor.example/v1" {
				t.Fatalf("rejected takeover changed the base: %v", live["chat"])
			}
			headers := live["headers"].(map[string]any)
			if headers[markerName] != "user-value" || headers["Authorization"] != "Bearer header-secret" {
				t.Fatalf("rejected takeover changed headers: %#v", headers)
			}
		})
	}
	var ledger supplierLedger
	ok, err := ctrl.store.getKV(supplierLedgerKey, &ledger)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ok && (len(ledger.Entries) != 0 || ledger.Pending != nil) {
		t.Fatalf("rejected takeover wrote ledger state: %#v", ledger)
	}
}

func TestSupplierHeaderContainerRestoration(t *testing.T) {
	type testCase struct {
		name           string
		headers        any
		hasHeaders     bool
		addHeaderLater bool
		wantAbsent     bool
		wantNull       bool
		wantEmpty      bool
	}
	cases := []testCase{
		{name: "absent", wantAbsent: true},
		{name: "null", headers: nil, hasHeaders: true, wantNull: true},
		{name: "empty object", headers: map[string]any{}, hasHeaders: true, wantEmpty: true},
		{name: "user header added while protected", wantAbsent: false, addHeaderLater: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, configDir, ctrl := newSupplierTestEnv(t)
			ctx := context.Background()
			provider := map[string]any{
				"id":   "plain",
				"name": "Plain",
				"chat": "https://plain.example/v1",
			}
			if tc.hasHeaders {
				provider["headers"] = tc.headers
			}
			supplierTestWriteProviders(t, configDir, map[string]any{"providers": []any{provider}})
			if err := ctrl.setProtection(ctx, "plain", true); err != nil {
				t.Fatalf("enable: %v", err)
			}
			live := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "plain")
			headers, ok := live["headers"].(map[string]any)
			if !ok || headers[supplierSessionHeader] != supplierSessionValue {
				t.Fatalf("probe header was not added: %#v", live["headers"])
			}

			if tc.addHeaderLater {
				data := supplierTestReadProviders(t, configDir)
				live = supplierTestProvider(t, data, "plain")
				headers = live["headers"].(map[string]any)
				headers["X-Added"] = "kept"
				supplierTestWriteProviders(t, configDir, data)
			}

			if err := ctrl.setProtection(ctx, "plain", false); err != nil {
				t.Fatalf("disable: %v", err)
			}
			live = supplierTestProvider(t, supplierTestReadProviders(t, configDir), "plain")
			restored, present := live["headers"]
			if tc.wantAbsent {
				if present {
					t.Fatalf("headers object was not removed when originally absent: %#v", restored)
				}
			} else if tc.wantNull {
				if !present || restored != nil {
					t.Fatalf("headers was not restored to null: present=%v value=%#v", present, restored)
				}
			} else if tc.wantEmpty {
				obj, ok := restored.(map[string]any)
				if !present || !ok || len(obj) != 0 {
					t.Fatalf("headers was not restored to {}: present=%v value=%#v", present, restored)
				}
			} else {
				obj, ok := restored.(map[string]any)
				if !present || !ok || obj["X-Added"] != "kept" {
					t.Fatalf("user header was not preserved: %#v", restored)
				}
				if _, marker := obj[supplierSessionHeader]; marker {
					t.Fatalf("probe header was not removed: %#v", obj)
				}
			}
			if live["chat"] != "https://plain.example/v1" {
				t.Fatalf("base not restored: %v", live["chat"])
			}
		})
	}
}

func TestSupplierNullAndEmptyProtocolsStayManagedButInactive(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	supplierTestWriteProviders(t, configDir, map[string]any{"providers": []any{map[string]any{
		"id":        "vendor",
		"name":      "Vendor",
		"chat":      "https://vendor.example/v1",
		"responses": nil,
		"anthropic": "",
	}}})

	if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	entry := supplierTestEntry(t, store, "vendor")
	if raw, ok := entry.Original["responses"]; !ok || !supplierJSONNull(raw) {
		t.Fatalf("null responses not kept in original: %q, present=%v", raw, ok)
	}
	if raw, ok := entry.Original["anthropic"]; !ok || string(raw) != `""` {
		t.Fatalf("empty anthropic not kept in original: %q, present=%v", raw, ok)
	}
	if _, ok := entry.Original["gemini"]; ok {
		t.Fatalf("absent gemini was recorded in original: %#v", entry.Original)
	}
	if raw, ok := entry.After["responses"]; !ok || !supplierJSONNull(raw) {
		t.Fatalf("null responses was not preserved while protected: %q, present=%v", raw, ok)
	}
	if raw, ok := entry.After["anthropic"]; !ok || string(raw) != `""` {
		t.Fatalf("empty anthropic was not preserved while protected: %q, present=%v", raw, ok)
	}
	if _, ok := entry.After["gemini"]; ok {
		t.Fatalf("absent gemini gained a managed value: %#v", entry.After)
	}

	live := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")
	if value, present := live["responses"]; !present || value != nil {
		t.Fatalf("responses changed while protected: present=%v value=%#v", present, value)
	}
	if value, present := live["anthropic"]; !present || value != "" {
		t.Fatalf("anthropic changed while protected: present=%v value=%#v", present, value)
	}
	if _, present := live["gemini"]; present {
		t.Fatalf("absent gemini appeared while protected: %#v", live)
	}
	if live["chat"] != supplierTestOrigin+"/upstream/"+entry.Token+"/chat/v1" {
		t.Fatalf("chat was not taken over: %v", live["chat"])
	}

	list, err := ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Providers) != 1 || !list.Providers[0].Protected || list.Providers[0].Conflict != "" {
		t.Fatalf("list = %#v", list.Providers)
	}
	if !reflect.DeepEqual(list.Providers[0].Protocols, []string{"chat"}) {
		t.Fatalf("inactive protocols were listed as protected: %v", list.Providers[0].Protocols)
	}
	for _, protocol := range []string{"responses", "anthropic", "gemini", "decide"} {
		if binding, ok, err := ctrl.binding(entry.Token, protocol); err != nil || ok {
			t.Fatalf("inactive %s produced a binding: %#v, %v, %v", protocol, binding, ok, err)
		}
	}
	if binding, ok, err := ctrl.binding(entry.Token, "chat"); err != nil || !ok || binding.Base != "https://vendor.example/v1" {
		t.Fatalf("chat binding = %#v, %v, %v", binding, ok, err)
	}
	if protected, err := ctrl.protectedProvider("vendor"); err != nil || !protected {
		t.Fatalf("protectedProvider = %v, %v; want true", protected, err)
	}

	// Filling in an originally empty protocol is a managed-field conflict, not
	// an unguarded route that the listing still calls protected.
	data := supplierTestReadProviders(t, configDir)
	liveProvider := supplierTestProvider(t, data, "vendor")
	liveProvider["anthropic"] = "https://evil.example"
	supplierTestWriteProviders(t, configDir, data)

	list, err = ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list after filling empty protocol: %v", err)
	}
	if len(list.Providers) != 1 || !list.Providers[0].Protected || !strings.Contains(list.Providers[0].Conflict, "anthropic") {
		t.Fatalf("filled empty protocol not reported: %#v", list.Providers)
	}
	if _, ok, err := ctrl.binding(entry.Token, "chat"); err == nil {
		t.Fatalf("binding still served with an unmanaged protocol: ok=%v", ok)
	}
	if _, _, err := ctrl.binding(entry.Token, "anthropic"); err == nil {
		t.Fatal("filled empty protocol was bound")
	}
	if protected, err := ctrl.protectedProvider("vendor"); err != nil || protected {
		t.Fatalf("conflicted provider reported protected: %v, %v", protected, err)
	}
	if err := ctrl.setProtection(ctx, "vendor", false); err == nil {
		t.Fatal("disable overwrote the filled-in protocol")
	}
	if err := ctrl.setProtection(ctx, "vendor", true); err == nil {
		t.Fatal("re-enable overwrote the filled-in protocol")
	}
	if got := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")["anthropic"]; got != "https://evil.example" {
		t.Fatalf("user edit was overwritten: %v", got)
	}

	data = supplierTestReadProviders(t, configDir)
	liveProvider = supplierTestProvider(t, data, "vendor")
	liveProvider["anthropic"] = ""
	supplierTestWriteProviders(t, configDir, data)
	if err := ctrl.setProtection(ctx, "vendor", false); err != nil {
		t.Fatalf("disable after reverting edit: %v", err)
	}

	stored := supplierTestRawProvider(t, configDir, "vendor")
	if got := string(stored["chat"]); got != `"https://vendor.example/v1"` {
		t.Fatalf("chat restore = %s", got)
	}
	if raw, present := stored["responses"]; !present || string(raw) != "null" {
		t.Fatalf("responses restore = %s, present=%v", raw, present)
	}
	if raw, present := stored["anthropic"]; !present || string(raw) != `""` {
		t.Fatalf("anthropic restore = %s, present=%v", raw, present)
	}
	for _, protocol := range []string{"gemini", "decide"} {
		if raw, present := stored[protocol]; present {
			t.Fatalf("absent %s was created on restore: %s", protocol, raw)
		}
	}
	if raw, present := stored["headers"]; present {
		t.Fatalf("absent headers were created on restore: %s", raw)
	}
	if ledger := supplierTestLedger(t, store); len(ledger.Entries) != 0 || ledger.Pending != nil {
		t.Fatalf("ledger not cleared: %#v", ledger)
	}
}

func TestSupplierAddedProtocolIsConflictWithoutBinding(t *testing.T) {
	store, configDir, ctrl := newSupplierTestEnv(t)
	ctx := context.Background()
	supplierTestWriteProviders(t, configDir, map[string]any{"providers": []any{map[string]any{
		"id":   "vendor",
		"name": "Vendor",
		"chat": "https://vendor.example/v1",
	}}})
	if err := ctrl.setProtection(ctx, "vendor", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	entry := supplierTestEntry(t, store, "vendor")
	for _, protocol := range []string{"responses", "anthropic", "gemini", "decide"} {
		if _, ok := entry.Original[protocol]; ok {
			t.Fatalf("absent %s recorded in original", protocol)
		}
		if _, ok := entry.After[protocol]; ok {
			t.Fatalf("absent %s gained a managed value", protocol)
		}
	}

	data := supplierTestReadProviders(t, configDir)
	addedProvider := supplierTestProvider(t, data, "vendor")
	addedProvider["responses"] = "https://added.example/v1"
	supplierTestWriteProviders(t, configDir, data)

	list, err := ctrl.list(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Providers) != 1 || !list.Providers[0].Protected {
		t.Fatalf("list = %#v", list.Providers)
	}
	if !strings.Contains(list.Providers[0].Conflict, "responses") {
		t.Fatalf("added protocol did not surface a conflict: %#v", list.Providers[0])
	}
	if !reflect.DeepEqual(list.Providers[0].Protocols, []string{"chat"}) {
		t.Fatalf("added protocol was listed as protected: %v", list.Providers[0].Protocols)
	}
	if binding, ok, err := ctrl.binding(entry.Token, "responses"); err != nil || ok {
		t.Fatalf("added protocol produced a binding: %#v, %v, %v", binding, ok, err)
	}
	if _, ok, err := ctrl.binding(entry.Token, "chat"); err == nil {
		t.Fatalf("binding still served despite the unmanaged protocol: ok=%v", ok)
	}
	if protected, err := ctrl.protectedProvider("vendor"); err != nil || protected {
		t.Fatalf("conflicted provider reported protected: %v, %v", protected, err)
	}
	if err := ctrl.reconcile(ctx); err == nil || !strings.Contains(err.Error(), "responses") {
		t.Fatalf("reconcile conflict = %v", err)
	}
	if got := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")["responses"]; got != "https://added.example/v1" {
		t.Fatalf("added protocol was overwritten: %v", got)
	}
	if err := ctrl.setProtection(ctx, "vendor", true); err == nil {
		t.Fatal("re-enable overwrote the added protocol")
	}
	if err := ctrl.setProtection(ctx, "vendor", false); err == nil {
		t.Fatal("disable overwrote the added protocol")
	}
	if got := supplierTestProvider(t, supplierTestReadProviders(t, configDir), "vendor")["responses"]; got != "https://added.example/v1" {
		t.Fatalf("user addition was overwritten: %v", got)
	}

	data = supplierTestReadProviders(t, configDir)
	reverted := supplierTestProvider(t, data, "vendor")
	delete(reverted, "responses")
	supplierTestWriteProviders(t, configDir, data)
	if err := ctrl.setProtection(ctx, "vendor", false); err != nil {
		t.Fatalf("disable after reverting addition: %v", err)
	}
	stored := supplierTestRawProvider(t, configDir, "vendor")
	if raw, present := stored["responses"]; present {
		t.Fatalf("added protocol was not removed on restore: %s", raw)
	}
	if got := string(stored["chat"]); got != `"https://vendor.example/v1"` {
		t.Fatalf("chat restore = %s", got)
	}
	if raw, present := stored["headers"]; present {
		t.Fatalf("absent headers were created on restore: %s", raw)
	}
	if ledger := supplierTestLedger(t, store); len(ledger.Entries) != 0 || ledger.Pending != nil {
		t.Fatalf("ledger not cleared: %#v", ledger)
	}
}

func TestSupplierListMissingFileIsNotConfigured(t *testing.T) {
	_, _, ctrl := newSupplierTestEnv(t)
	list, err := ctrl.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if list.Configured || len(list.Providers) != 0 || list.Error != "" {
		t.Fatalf("missing providers.json list = %#v", list)
	}
	if err := ctrl.setProtection(context.Background(), "vendor", true); err == nil {
		t.Fatal("setProtection succeeded without providers.json")
	}
	if _, ok, err := ctrl.binding("000000000000000000000000000000000000000000000000", "chat"); err != nil || ok {
		t.Fatalf("unknown token with missing file: ok=%v err=%v", ok, err)
	}
}
