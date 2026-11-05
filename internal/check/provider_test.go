package check

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

type nativeRequest struct {
	method  string
	path    string
	escaped string
	query   string
	header  http.Header
	body    []byte
}

type nativeRecorder struct {
	mu       sync.Mutex
	requests []nativeRequest
}

func (n *nativeRecorder) record(r *http.Request, body []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.requests = append(n.requests, nativeRequest{
		method:  r.Method,
		path:    r.URL.Path,
		escaped: r.URL.EscapedPath(),
		query:   r.URL.RawQuery,
		header:  r.Header.Clone(),
		body:    body,
	})
}

func (n *nativeRecorder) last() nativeRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.requests) == 0 {
		return nativeRequest{}
	}
	return n.requests[len(n.requests)-1]
}

func (n *nativeRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.requests)
}

type nativeHandler func(w http.ResponseWriter, r *http.Request, body []byte)

// supplierEnv drives the supplier takeover endpoint. Magpie itself is not part
// of these tests: the vendor server plays the role of the prepared upstream,
// and the managed providers.json base URLs are used exactly as Magpie would.
type supplierEnv struct {
	svc       *Service
	store     *Store
	vendor    *httptest.Server
	guard     *httptest.Server
	rec       *nativeRecorder
	configDir string
	bases     map[string]string
}

var supplierVendorPaths = map[string]string{
	"chat":      "/chat/v1",
	"responses": "/responses/v1",
	"anthropic": "/anthropic",
	"gemini":    "/gemini/v1beta",
}

func newSupplierEnv(t *testing.T, handler nativeHandler) *supplierEnv {
	t.Helper()
	svc, store := newRuleTestService(t)
	rec := &nativeRecorder{}
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(r, body)
		handler(w, r, body)
	}))
	t.Cleanup(vendor.Close)

	configDir := filepath.Join(t.TempDir(), "magpie")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	providers := map[string]any{"providers": []any{map[string]any{
		"id":        "vendor",
		"name":      "Vendor",
		"preset":    "custom",
		"chat":      vendor.URL + "/chat/v1",
		"responses": vendor.URL + "/responses/v1",
		"anthropic": vendor.URL + "/anthropic/v1",
		"gemini":    vendor.URL + "/gemini/v1beta",
		"models":    []any{"vendor/big"},
	}}}
	raw, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		t.Fatalf("marshal providers: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "providers.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write providers: %v", err)
	}
	// Magpie's wire name is a vendor-specific mapping; keep it equal to the
	// monitored model prefix so business requests can select a target.
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
		[]byte(`{"modelWires":{"vendor/*":"vendor/*"}}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	guard := httptest.NewServer(registerGuardRoutes(http.NewServeMux(), svc))
	t.Cleanup(guard.Close)
	svc.suppliers = newSupplierController(store, configDir, guard.URL)
	if err := svc.suppliers.setProtection(context.Background(), "vendor", true); err != nil {
		t.Fatalf("enable protection: %v", err)
	}
	managed, err := os.ReadFile(filepath.Join(configDir, "providers.json"))
	if err != nil {
		t.Fatalf("read managed providers: %v", err)
	}
	var snapshot struct {
		Providers []struct {
			Chat      string `json:"chat"`
			Responses string `json:"responses"`
			Anthropic string `json:"anthropic"`
			Gemini    string `json:"gemini"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(managed, &snapshot); err != nil {
		t.Fatalf("unmarshal managed providers: %v", err)
	}
	if len(snapshot.Providers) != 1 {
		t.Fatalf("managed providers=%d", len(snapshot.Providers))
	}
	provider := snapshot.Providers[0]
	env := &supplierEnv{
		svc: svc, store: store, vendor: vendor, guard: guard, rec: rec, configDir: configDir,
		bases: map[string]string{
			"chat":      provider.Chat,
			"responses": provider.Responses,
			"anthropic": provider.Anthropic,
			"gemini":    provider.Gemini,
		},
	}
	env.configure(t, 2, nil)
	return env
}

func (e *supplierEnv) configure(t *testing.T, attempts int, mutate func(*GuardConfig)) {
	t.Helper()
	cfg := mustConfig(t, e.store)
	cfg.GatewayURL = e.vendor.URL
	cfg.GatewayKey = "gateway-key"
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ProviderAttempts = attempts
	if mutate != nil {
		mutate(&cfg)
	}
	if err := e.svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func (e *supplierEnv) path(protocol, suffix string) string {
	return e.bases[protocol] + suffix
}

func (e *supplierEnv) token(t *testing.T) string {
	t.Helper()
	rest := strings.TrimPrefix(e.bases["chat"], e.guard.URL+"/upstream/")
	token, _, ok := strings.Cut(rest, "/")
	if !ok || token == "" {
		t.Fatalf("managed chat base %q has no route token", e.bases["chat"])
	}
	return token
}

func (e *supplierEnv) saveTarget(t *testing.T, target *Target) {
	t.Helper()
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	if err := e.store.SaveTarget(target); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}
}

func (e *supplierEnv) disableProtection(t *testing.T) {
	t.Helper()
	if err := e.svc.suppliers.setProtection(context.Background(), "vendor", false); err != nil {
		t.Fatalf("disable protection: %v", err)
	}
}

func (e *supplierEnv) call(t *testing.T, method, target string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func jsonOK(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func customTarget(model string, rule ResponseModelRule) *Target {
	return &Target{
		Model: model, Enabled: true,
		ResponseRuleMode:   ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{rule},
	}
}

func newSupplierRuleEnv(t *testing.T, handler nativeHandler, rule ResponseModelRule, attempts int) *supplierEnv {
	t.Helper()
	env := newSupplierEnv(t, handler)
	env.saveTarget(t, customTarget("vendor/big", rule))
	env.configure(t, attempts, nil)
	return env
}

// routeCase exercises the vendor wire mapping Magpie prepares for one endpoint.
type routeCase struct {
	name         string
	protocol     string
	suffix       string
	nativeSuffix string
	method       string
	body         string
	query        string
}

func TestSupplierProxyForwardsNativeRoutes(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"ok":true}`)
	})
	cases := []routeCase{
		{name: "chat completions", protocol: "chat", suffix: "/chat/completions", nativeSuffix: "/chat/completions", method: http.MethodPost, body: `{"model":"vendor/ok","messages":[]}`},
		{name: "responses", protocol: "responses", suffix: "/responses", nativeSuffix: "/responses", method: http.MethodPost, body: `{"model":"vendor/ok","input":"hi"}`},
		{name: "responses compact", protocol: "responses", suffix: "/responses/compact", nativeSuffix: "/responses/compact", method: http.MethodPost, body: `{"model":"vendor/ok"}`},
		{name: "anthropic messages", protocol: "anthropic", suffix: "/v1/messages", nativeSuffix: "/v1/messages", method: http.MethodPost, body: `{"model":"vendor/ok","messages":[]}`},
		{name: "anthropic count tokens", protocol: "anthropic", suffix: "/v1/messages/count_tokens", nativeSuffix: "/v1/messages/count_tokens", method: http.MethodPost, body: `{"model":"vendor/ok"}`},
		{name: "gemini generate", protocol: "gemini", suffix: "/models/gemini-2.5-flash:generateContent", nativeSuffix: "/models/gemini-2.5-flash:generateContent", method: http.MethodPost, body: `{"contents":[]}`},
		{name: "gemini escaped stream", protocol: "gemini", suffix: "/models/google%2Fgemini:streamGenerateContent", nativeSuffix: "/models/google/gemini:streamGenerateContent", method: http.MethodPost, body: `{"contents":[]}`, query: "alt=sse"},
		{name: "gemini count tokens", protocol: "gemini", suffix: "/models/gemini-2.5-flash:countTokens", nativeSuffix: "/models/gemini-2.5-flash:countTokens", method: http.MethodPost, body: `{"contents":[]}`},
		{name: "system one", protocol: "responses", suffix: "/systemone", nativeSuffix: "/systemone", method: http.MethodPost, body: `{}`},
		{name: "alpha search", protocol: "chat", suffix: "/alpha/search", nativeSuffix: "/alpha/search", method: http.MethodPost, body: `{}`},
		{name: "images generations", protocol: "chat", suffix: "/images/generations", nativeSuffix: "/images/generations", method: http.MethodPost, body: `{}`},
		{name: "images edits", protocol: "chat", suffix: "/images/edits", nativeSuffix: "/images/edits", method: http.MethodPost, body: "raw-multipart-bytes"},
		{name: "embeddings", protocol: "chat", suffix: "/embeddings", nativeSuffix: "/embeddings", method: http.MethodPost, body: `{}`},
		{name: "rerank", protocol: "chat", suffix: "/rerank", nativeSuffix: "/rerank", method: http.MethodPost, body: `{}`},
		{name: "audio transcriptions", protocol: "chat", suffix: "/audio/transcriptions", nativeSuffix: "/audio/transcriptions", method: http.MethodPost, body: "audio-bytes"},
		{name: "audio speech", protocol: "chat", suffix: "/audio/speech", nativeSuffix: "/audio/speech", method: http.MethodPost, body: `{}`},
		{name: "videos create", protocol: "chat", suffix: "/videos", nativeSuffix: "/videos", method: http.MethodPost, body: `{}`},
		{name: "video poll", protocol: "chat", suffix: "/videos/vid_123", nativeSuffix: "/videos/vid_123", method: http.MethodGet},
		{name: "video content", protocol: "chat", suffix: "/videos/vid_123/content", nativeSuffix: "/videos/vid_123/content", method: http.MethodGet},
		{name: "models catalog", protocol: "chat", suffix: "/models", nativeSuffix: "/models", method: http.MethodGet},
		{name: "model by escaped id", protocol: "chat", suffix: "/models/vendor%2Fextra", nativeSuffix: "/models/vendor/extra", method: http.MethodGet},
		{name: "codex models", protocol: "chat", suffix: "/codex/models", nativeSuffix: "/codex/models", method: http.MethodGet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := env.path(tc.protocol, tc.suffix)
			if tc.query != "" {
				target += "?" + tc.query
			}
			headers := map[string]string{
				"Content-Type":          "application/json",
				"Authorization":         "Bearer vendor-key",
				"Cookie":                "vendor-session=selected",
				"Proxy-Authorization":   "Basic x",
				"X-Forwarded-For":       "10.0.0.1",
				"Anthropic-Version":     "2023-06-01",
				"X-Magpie-Session":      "session-1",
				"X-Model-Guard-Session": "must-not-leak",
			}
			if tc.name == "images edits" || tc.name == "audio transcriptions" {
				headers["Content-Type"] = "multipart/form-data; boundary=abc123"
			}
			before := env.rec.count()
			resp := env.call(t, tc.method, target, []byte(tc.body), headers)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s status=%d", tc.method, target, resp.StatusCode)
			}
			if got := env.rec.count(); got != before+1 {
				t.Fatalf("native calls=%d, want %d", got, before+1)
			}
			got := env.rec.last()
			wantPath := supplierVendorPaths[tc.protocol] + tc.nativeSuffix
			if got.method != tc.method || got.path != wantPath {
				t.Fatalf("native got %s %s, want %s %s", got.method, got.path, tc.method, wantPath)
			}
			if got.query != tc.query {
				t.Fatalf("native query=%q, want %q", got.query, tc.query)
			}
			if got.header.Get("Authorization") != "Bearer vendor-key" {
				t.Fatalf("vendor Authorization=%q", got.header.Get("Authorization"))
			}
			if got.header.Get("Cookie") != "vendor-session=selected" {
				t.Fatalf("vendor Cookie=%q", got.header.Get("Cookie"))
			}
			for _, removed := range []string{"Proxy-Authorization", "X-Forwarded-For", "X-Model-Guard-Session"} {
				if value := got.header.Get(removed); value != "" {
					t.Fatalf("%s leaked upstream: %q", removed, value)
				}
			}
			if got.header.Get("Anthropic-Version") != "2023-06-01" || got.header.Get("X-Magpie-Session") != "session-1" {
				t.Fatalf("end-to-end headers dropped: %v", got.header)
			}
			if tc.method == http.MethodPost && !bytes.Equal(got.body, []byte(tc.body)) {
				t.Fatalf("native body=%q, want %q", got.body, tc.body)
			}
			if strings.Contains(string(got.body), ProbeAliasPrefix) {
				t.Fatal("business request must keep its original model")
			}
		})
	}
}

func TestSupplierProxyBinaryResponsesPassThrough(t *testing.T) {
	video := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	audio := []byte("ID3-audio-bytes")
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/content"):
			if r.Header.Get("Range") != "bytes=0-4" {
				t.Errorf("Range dropped: %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 0-4/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(video)
		default:
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(audio)
		}
	})
	resp := env.call(t, http.MethodGet, env.path("chat", "/videos/vid_123/content"), nil, map[string]string{"Range": "bytes=0-4"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, video) {
		t.Fatalf("video status=%d len=%d", resp.StatusCode, len(body))
	}
	if resp.Header.Get("Content-Range") != "bytes 0-4/5" {
		t.Fatalf("Content-Range=%q", resp.Header.Get("Content-Range"))
	}
	resp = env.call(t, http.MethodPost, env.path("chat", "/audio/speech"), []byte(`{}`), map[string]string{"Content-Type": "application/json"})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, audio) || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("audio status=%d type=%q body=%q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
}

func TestSupplierProxyRejectsUnboundUpstreamPaths(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unbound path reached vendor: %s", r.URL.Path)
	})
	token := env.token(t)
	for _, path := range []string{
		"/upstream",
		"/upstream/" + token,
		"/upstream/not-a-token/chat/v1/models",
		"/upstream/" + token + "/decide/v1/models",
	} {
		req, err := http.NewRequest(http.MethodGet, env.guard.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, resp.StatusCode)
		}
	}
	if got := env.rec.count(); got != 0 {
		t.Fatalf("vendor calls=%d, want 0", got)
	}
}

func TestSupplierCatalogPassthrough(t *testing.T) {
	catalog := `{"object":"list","data":[{"id":"vendor/unknown","object":"model","context_window":128000,` +
		`"modalities":["text","image"],"reasoning_levels":["low","high"],"native_endpoints":{"chat":true},` +
		`"pricing":{"input":1},"service_tiers":["flex"],"future_field":{"nested":true}}],"has_more":false}`
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Extra", "keep")
		_, _ = io.WriteString(w, catalog)
	})
	resp := env.call(t, http.MethodGet, env.path("chat", "/models"), nil, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != catalog {
		t.Fatalf("catalog altered:\n%s", body)
	}
	if resp.Header.Get("X-Extra") != "keep" {
		t.Fatal("end-to-end response header dropped")
	}
}

func TestSupplierNativeErrorsPassThrough(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusNotFound} {
		env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "7")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"native"}}`)
		})
		resp := env.call(t, http.MethodGet, env.path("chat", "/models"), nil, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != status || !strings.Contains(string(body), "native") {
			t.Fatalf("status=%d body=%s, want %d", resp.StatusCode, body, status)
		}
		if status == http.StatusTooManyRequests && resp.Header.Get("Retry-After") != "7" {
			t.Fatalf("Retry-After=%q", resp.Header.Get("Retry-After"))
		}
	}
}

func TestSupplierUnmonitoredModelIsCallable(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/other"}`)
	})
	env.saveTarget(t, &Target{Model: "vendor/big", Enabled: true})
	body := []byte(`{"model":"vendor/other"}`)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"), body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := env.rec.last().body; string(got) != string(body) {
		t.Fatalf("model rewritten: %s", got)
	}
}

func TestSupplierBlockedTargetStopsBeforeUpstream(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("blocked target reached vendor")
	})
	env.saveTarget(t, &Target{Model: "vendor/big", Enabled: true, State: StateFailed})
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("blocked target must not call vendor")
	}
}

func TestSupplierProtocolsJudgeVendorModel(t *testing.T) {
	type protocolCase struct {
		name     string
		protocol string
		suffix   string
		request  string
		respond  func(w http.ResponseWriter, model string)
		want     string
	}
	cases := []protocolCase{
		{
			name: "chat", protocol: "chat", suffix: "/chat/completions",
			request: `{"model":"vendor/big","messages":[]}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"model":%q,"choices":[{"message":{"content":"ok"}}]}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "responses", protocol: "responses", suffix: "/responses",
			request: `{"model":"vendor/big","input":"hi"}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"object":"response","response":{"model":%q}}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "anthropic", protocol: "anthropic", suffix: "/v1/messages",
			request: `{"model":"vendor/big","messages":[]}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"type":"message","message":{"model":%q}}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "gemini", protocol: "gemini", suffix: "/models/vendor%2Fbig:generateContent",
			request: `{"contents":[]}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"modelVersion":%q,"candidates":[]}`, model))
			},
			want: `"vendor/big"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var env *supplierEnv
			env = newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				calls.Add(1)
				if r.Header.Get("X-Magpie-Response-Model") != "vendor" {
					t.Errorf("vendor naming preference lost: %q", r.Header.Get("X-Magpie-Response-Model"))
				}
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("judged request must ask for identity encoding, got %q", r.Header.Get("Accept-Encoding"))
				}
				if calls.Load() == 1 {
					tc.respond(w, "vendor/flash")
					return
				}
				tc.respond(w, "vendor/big")
			})
			env.saveTarget(t, customTarget("vendor/big", ResponseModelRule{
				ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
			}))
			env.configure(t, 2, nil)
			resp := env.call(t, http.MethodPost, env.path(tc.protocol, tc.suffix),
				[]byte(tc.request), map[string]string{"X-Magpie-Response-Model": "vendor"})
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), tc.want) {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls=%d, want 2", calls.Load())
			}
			target, _ := env.store.Target("vendor/big")
			if target == nil || target.State == StateFailed {
				t.Fatalf("target must stay usable after a good retry: %+v", target)
			}
		})
	}
}

func TestSupplierRecordDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		jsonOK(w, `{"model":"vendor/flash"}`)
	}, ResponseModelRule{
		ID: "r-record", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord,
	}, 2)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State == StateFailed {
		t.Fatalf("record must not disable: %+v", target)
	}
}

func TestSupplierDisablesAfterAttempts(t *testing.T) {
	var calls atomic.Int32
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		jsonOK(w, `{"model":"vendor/flash"}`)
	}, ResponseModelRule{
		ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}, 2)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "已尝试 2 次") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q", resp.Header.Get("Retry-After"))
	}
	if calls.Load() != 2 {
		t.Fatalf("vendor calls=%d, want 2", calls.Load())
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || !target.Blocked || target.BlockSource != BlockSourceRule || target.RuleHoldUntil.IsZero() {
		t.Fatalf("target not rule-blocked: %+v", target)
	}
	if target.State == StateFailed {
		t.Fatalf("rule hold must not rewrite probe state: %+v", target)
	}
}

func TestSupplierRetriesStreaming(t *testing.T) {
	var calls atomic.Int32
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		model := "vendor/flash"
		if n > 1 {
			model = "vendor/big"
		}
		_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", model)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}, ResponseModelRule{
		ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}, 2)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big","stream":true}`), nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"vendor/big"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("vendor calls=%d, want 2", calls.Load())
	}
}

func TestSupplierProtectionOffReturnsNotFound(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("disabled protection reached vendor")
	})
	target := env.path("chat", "/models")
	env.disableProtection(t)
	resp := env.call(t, http.MethodGet, target, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled protection status=%d, want 404", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("disabled protection forwarded a request")
	}
}

func TestProviderJSONModelVariants(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"vendor/flash"}`, "vendor/flash"},
		{`{"modelVersion":"gemini-2.5-flash"}`, "gemini-2.5-flash"},
		{`{"response":{"model":"vendor/nested"}}`, "vendor/nested"},
		{`{"message":{"model":"vendor/anthropic"}}`, "vendor/anthropic"},
		{`{"candidates":[{"modelVersion":"gemini-candidate"}]}`, "gemini-candidate"},
		{`{"model":"first","model":"second"}`, "first"},
		{`{"model":123}`, ""},
		{`{"model":"a\u002fb"}`, "a/b"},
	}
	for _, tc := range cases {
		if got := providerJSONModel([]byte(tc.body)); got != tc.want {
			t.Fatalf("providerJSONModel(%s)=%q, want %q", tc.body, got, tc.want)
		}
	}
}

// testBody is a controllable request body for limit and idle tests.
type testBody struct {
	data     []byte
	offset   int
	block    bool
	closed   chan struct{}
	closeOne sync.Once
}

func (b *testBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	if b.block {
		<-b.closed
		return 0, io.ErrClosedPipe
	}
	return 0, io.EOF
}

func (b *testBody) Close() error {
	if b.closed != nil {
		b.closeOne.Do(func() { close(b.closed) })
	}
	return nil
}

func TestProviderUnsupportedEncoding(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unsupported encoding reached vendor")
	})
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), map[string]string{"Content-Encoding": "br"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("unsupported encoding must not reach vendor")
	}
}

func TestProviderBadJSON(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("bad JSON reached vendor")
	})
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestProviderBodyLimit(t *testing.T) {
	old := providerBodyLimit
	providerBodyLimit = 1024
	t.Cleanup(func() { providerBodyLimit = old })

	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("oversized body reached vendor")
	})
	body := []byte(`{"model":"vendor/big","pad":"` + strings.Repeat("a", 2048) + `"}`)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"), body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("oversized body must not reach vendor")
	}
}

func TestProviderIdleTimeout(t *testing.T) {
	old := providerIdleRead
	providerIdleRead = 50 * time.Millisecond
	t.Cleanup(func() { providerIdleRead = old })

	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("timed out body reached vendor")
	})
	body := &testBody{data: []byte(`{"model":"vendor/big"`), block: true, closed: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, env.path("chat", "/chat/completions"), body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.svc.apiSupplier(rec, req)
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status=%d, want 408", rec.Code)
	}
	if env.rec.count() != 0 {
		t.Fatal("timed out body must not reach vendor")
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func TestProviderRequestEncodingReplayedVerbatim(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/big"}`)
	})
	inner := []byte(`{"model":"vendor/big","tools":[{"type":"function","function":{"name":"x"}}],"signature":"abc+/=","unknown":{"deep":[1,2,3]}}`)
	for _, tc := range []struct {
		name     string
		encoding string
		body     []byte
	}{
		{"gzip", "gzip", gzipBytes(t, inner)},
		{"zstd", "zstd", zstdBytes(t, inner)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"), tc.body,
				map[string]string{"Content-Encoding": tc.encoding, "Content-Type": "application/json"})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			got := env.rec.last()
			if !bytes.Equal(got.body, tc.body) {
				t.Fatalf("vendor received re-encoded body")
			}
			if got.header.Get("Content-Encoding") != tc.encoding {
				t.Fatalf("Content-Encoding=%q", got.header.Get("Content-Encoding"))
			}
		})
	}
}

func TestProviderLargeBodyBeyond32MiB(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/big"}`)
	})
	pad := strings.Repeat("b", 33<<20)
	body := []byte(`{"model":"vendor/big","pad":"` + pad + `"}`)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"), body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	if got := env.rec.last(); !bytes.Equal(got.body, body) {
		t.Fatalf("large body altered: got %d bytes, want %d", len(got.body), len(body))
	}
}

func TestProviderStreamPrefixVariants(t *testing.T) {
	oldWait := providerPrefixWait
	providerPrefixWait = 100 * time.Millisecond
	t.Cleanup(func() { providerPrefixWait = oldWait })

	cases := []struct {
		name   string
		stream string
	}{
		{
			name:   "multiline CRLF with comment",
			stream: ": keepalive\r\nevent: message\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\r\ndata: \"model\":\"vendor/big\"}\r\n\r\n",
		},
		{
			name:   "keepalive only then model",
			stream: ": ping\n\ndata: {\"model\":\"vendor/big\"}\n\n",
		},
		{
			name:   "no model",
			stream: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}, ResponseModelRule{
				ID: "r-none", Match: ResponseMatchContains, Pattern: "never", Action: ResponseActionRecord,
			}, 2)
			resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
				[]byte(`{"model":"vendor/big"}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != tc.stream {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
			if env.rec.count() != 1 {
				t.Fatalf("calls=%d, want 1 (no matching model must not retry)", env.rec.count())
			}
			target, _ := env.store.Target("vendor/big")
			if target == nil || target.State == StateFailed {
				t.Fatalf("unmatched stream changed target: %+v", target)
			}
		})
	}
}

func TestProviderStreamOversizeEventPassesThrough(t *testing.T) {
	old := providerPrefixLimit
	providerPrefixLimit = 256
	t.Cleanup(func() { providerPrefixLimit = old })

	stream := "data: {\"model\":\"vendor/big\",\"pad\":\"" + strings.Repeat("x", 1024) + "\"}\n\n"
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}, ResponseModelRule{
		ID: "r-none", Match: ResponseMatchContains, Pattern: "never", Action: ResponseActionRecord,
	}, 2)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != stream {
		t.Fatalf("status=%d len=%d", resp.StatusCode, len(body))
	}
}

func TestInspectProviderPrefixBoundaryAndError(t *testing.T) {
	old := providerPrefixWait
	providerPrefixWait = 60 * time.Millisecond
	t.Cleanup(func() { providerPrefixWait = old })

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	go func() { _, _ = pw.Write([]byte(": ping\n\n")) }()
	start := time.Now()
	prefix, rest, model, err := inspectProviderPrefix(t.Context(), pr, providerStreamSSE)
	if err != nil || model != "" {
		t.Fatalf("prefix model=%q err=%v", model, err)
	}
	if string(prefix) != ": ping\n\n" {
		t.Fatalf("prefix=%q", prefix)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("boundary took %v", elapsed)
	}
	go func() { _, _ = pw.Write([]byte("data: {\"model\":\"vendor/big\"}\n\n")); _ = pw.Close() }()
	got, err := io.ReadAll(rest)
	if err != nil || !strings.Contains(string(got), "vendor/big") {
		t.Fatalf("continuation=%q err=%v", got, err)
	}
	_ = rest.Close()

	errBody := &failingBody{data: []byte("data: partial"), err: io.ErrUnexpectedEOF}
	_, _, _, err = inspectProviderPrefix(t.Context(), errBody, providerStreamSSE)
	if err == nil {
		t.Fatal("non-EOF read failure must be reported")
	}
}

type failingBody struct {
	data []byte
	done bool
	err  error
}

func (f *failingBody) Read(p []byte) (int, error) {
	if !f.done {
		f.done = true
		n := copy(p, f.data)
		return n, nil
	}
	return 0, f.err
}

func (f *failingBody) Close() error { return nil }

func TestProviderStreamFlushesChunks(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce, doneOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	finishUpstream := func() { doneOnce.Do(func() { close(done) }) }

	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		defer close(finished)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\"vendor/big\",\"marker\":\"first\"}\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"model\":\"vendor/big\",\"marker\":\"second\"}\n\n")
		flusher.Flush()
		<-done
	}, ResponseModelRule{
		ID: "r-none", Match: ResponseMatchContains, Pattern: "never", Action: ResponseActionRecord,
	}, 2)
	defer func() {
		releaseUpstream()
		finishUpstream()
	}()

	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big","stream":true}`), nil)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	readUntil := func(marker string) string {
		ch := make(chan error, 1)
		var line string
		go func() {
			for {
				got, err := reader.ReadString('\n')
				if err != nil {
					ch <- err
					return
				}
				if strings.Contains(got, marker) {
					line = got
					ch <- nil
					return
				}
			}
		}()
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("waiting for %q: %v", marker, err)
			}
			return line
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %q before vendor EOF", marker)
			return ""
		}
	}

	readUntil("first")
	releaseUpstream()
	readUntil("second")
	finishUpstream()
	_, _ = io.Copy(io.Discard, reader)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("vendor did not finish")
	}
}

func TestSupplierActionFailureReturns500(t *testing.T) {
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/flash"}`)
	}, ResponseModelRule{
		ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}, 2)
	if _, err := env.store.db.Exec(`CREATE TRIGGER fail_targets BEFORE UPDATE ON targets BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "规则动作提交失败") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Fatal("action failure must not advertise Retry-After")
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.Blocked || !target.RuleHoldUntil.IsZero() {
		t.Fatalf("failed hold must not be reported as applied: %+v", target)
	}
	if n := supplierEventCount(t, env.store, "action", "vendor/big"); n != 0 {
		t.Fatalf("failed action was logged as applied: %d entries", n)
	}
	if n := supplierEventCount(t, env.store, "error", "vendor/big"); n != 1 {
		t.Fatalf("failed action error log entries = %d", n)
	}
	// The dedupe entry was not written, so a repaired guard can retry the action.
	if _, err := env.store.db.Exec(`DROP TRIGGER fail_targets`); err != nil {
		t.Fatal(err)
	}
	resp = env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("retry after repair status=%d body=%s", resp.StatusCode, body)
	}
	target, _ = env.store.Target("vendor/big")
	if target == nil || !target.Blocked || target.BlockSource != BlockSourceRule {
		t.Fatalf("recovered submission must hold the target: %+v", target)
	}
	if n := supplierEventCount(t, env.store, "action", "vendor/big"); n != 1 {
		t.Fatalf("recovered action log entries = %d", n)
	}
}

func TestSupplierRerouteFailurePreservesExistingKVs(t *testing.T) {
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/flash"}`)
	}, ResponseModelRule{
		ID: "r-reroute", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionReroute,
	}, 2)
	if err := env.store.AddResponseReroute("vendor/keep", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TRIGGER rr_insert BEFORE INSERT ON kv WHEN NEW.key='response_reroutes' BEGIN SELECT RAISE(ABORT,'boom'); END`,
		`CREATE TRIGGER rr_update BEFORE UPDATE ON kv WHEN NEW.key='response_reroutes' BEGIN SELECT RAISE(ABORT,'boom'); END`,
	} {
		if _, err := env.store.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "规则动作提交失败") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	routes, err := env.store.ResponseReroutes(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != "vendor/keep" {
		t.Fatalf("existing reroutes changed: %v", routes)
	}
	if _, err := env.store.db.Exec(`DROP TRIGGER rr_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.db.Exec(`DROP TRIGGER rr_update`); err != nil {
		t.Fatal(err)
	}
	resp = env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("recovered reroute status=%d retry=%q body=%s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	routes, err = env.store.ResponseReroutes(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(routes, ",")
	if !strings.Contains(joined, "vendor/keep") || !strings.Contains(joined, "vendor/big") {
		t.Fatalf("reroutes=%v", routes)
	}
}

func TestSupplierDecodesCompressedResponseOnlyWithRules(t *testing.T) {
	inner := []byte(`{"model":"vendor/big","choices":[]}`)
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(gzipBytes(t, inner))
	}, ResponseModelRule{
		ID: "r-none", Match: ResponseMatchContains, Pattern: "none", Action: ResponseActionRecord,
	}, 2)
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, inner) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("decoded delivery kept Content-Encoding=%q", resp.Header.Get("Content-Encoding"))
	}
}

func TestSupplierTransparentPathKeepsCompression(t *testing.T) {
	inner := []byte(`{"model":"vendor/big","choices":[]}`)
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(gzipBytes(t, inner))
	})
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"vendor/big"}`), map[string]string{"Accept-Encoding": "gzip"})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(raw, gzipBytes(t, inner)) {
		t.Fatalf("transparent response altered: encoding=%q len=%d", resp.Header.Get("Content-Encoding"), len(raw))
	}
}

func TestSupplierHeadSemantics(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"object":"list","data":[]}`)
	})
	resp := env.call(t, http.MethodHead, env.path("chat", "/models"), nil, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := env.rec.last(); got.method != http.MethodHead {
		t.Fatalf("vendor method=%s", got.method)
	}
}

func TestSupplierOllamaNeverJudgesResponseModel(t *testing.T) {
	stream := `{"model":"vendor/bad","message":{"role":"assistant","content":"ok"},"done":false}` + "\n" +
		`{"model":"vendor/bad","done":true}` + "\n"
	var calls atomic.Int32
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, stream)
	})
	env.saveTarget(t, customTarget("vendor/bad", ResponseModelRule{
		ID: "r-bad", Match: ResponseMatchContains, Pattern: "vendor/bad", Action: ResponseActionDisable,
	}))
	env.configure(t, 2, nil)
	resp := env.call(t, http.MethodPost, env.path("chat", "/api/chat"),
		[]byte(`{"model":"vendor/bad","messages":[]}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != stream {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
	if got := env.rec.last().header.Get("X-Magpie-Response-Model"); got != "" {
		t.Fatalf("Ollama request must not ask for vendor naming: %q", got)
	}
	target, _ := env.store.Target("vendor/bad")
	if target == nil || target.State == StateFailed {
		t.Fatalf("Ollama request model must not trigger a response action: %+v", target)
	}
}

func TestSupplierUnmonitoredRequestUsesOnlyGlobalRules(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/flash"}`)
	})
	env.saveTarget(t, customTarget("vendor/monitored", ResponseModelRule{
		ID: "r-other", Match: ResponseMatchContains, Pattern: "xyz", Action: ResponseActionDisable,
	}))
	env.configure(t, 2, func(cfg *GuardConfig) {
		cfg.ResponseRules = []ResponseModelRule{{
			ID: "r-global", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}}
	})
	resp := env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
		[]byte(`{"model":"unmonitored"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	monitored, _ := env.store.Target("vendor/monitored")
	if monitored == nil || monitored.State == StateFailed {
		t.Fatalf("a different monitored target must not be disabled: %+v", monitored)
	}
	events, _, err := env.store.SupplierEvents(10, "")
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0].Kind != "action" || events[0].RuleID != "r-global" || !strings.Contains(events[0].Detail, "未找到监测目标，未阻断") {
		t.Fatalf("event=%+v", events[0])
	}
	if results, _, err := env.store.Results("vendor/unmonitored", "", 10, ""); err != nil || len(results) != 0 {
		t.Fatalf("rule hit leaked into probe history: %v err=%v", results, err)
	}
}

func TestSupplierKeepsRuleSnapshotForInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	env := newSupplierRuleEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		jsonOK(w, `{"model":"vendor/flash"}`)
	}, ResponseModelRule{
		ID: "r-record", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord,
	}, 2)

	done := make(chan *http.Response, 1)
	go func() {
		done <- env.call(t, http.MethodPost, env.path("chat", "/chat/completions"),
			[]byte(`{"model":"vendor/big"}`), nil)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach vendor")
	}
	// The rule changes while the request is in flight; the snapshot must win.
	if err := env.store.SaveTarget(customTarget("vendor/big", ResponseModelRule{
		ID: "r-disable", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	})); err != nil {
		t.Fatal(err)
	}
	close(release)
	resp := <-done
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State == StateFailed {
		t.Fatalf("in-flight rule snapshot changed: %+v", target)
	}
	events, _, err := env.store.SupplierEvents(10, "")
	if err != nil || len(events) == 0 || events[0].RuleID != "r-record" || events[0].Action != ResponseActionRecord {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if results, _, err := env.store.Results("vendor/big", "", 10, ""); err != nil || len(results) != 0 {
		t.Fatalf("rule hit leaked into probe history: %v err=%v", results, err)
	}
}

func TestProviderJSONRequestModel(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"vendor/big"}`, "vendor/big"},
		{`{"type":"response.create","model":"vendor/big","input":[]}`, "vendor/big"},
		{`{"message":{"model":"vendor/nested"},"model":"vendor/top"}`, "vendor/top"},
		{`{"model":"first","model":"last"}`, "last"},
		{`{"message":{"model":"vendor/nested"}}`, ""},
	}
	for _, tc := range cases {
		values, err := scanProviderJSONRequest(strings.NewReader(tc.body), int64(len(tc.body))+1)
		if err != nil {
			t.Fatalf("scan request %s: %v", tc.body, err)
		}
		if values.Model != tc.want {
			t.Fatalf("request model %s=%q, want %q", tc.body, values.Model, tc.want)
		}
	}
}

func TestProviderJSONResponseIDs(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		request bool
		want    string
	}{
		{"chat whole", `{"id":"chatcmpl-a","model":"m"}`, false, "chatcmpl-a"},
		{"anthropic whole", `{"id":"msg_a","model":"m"}`, false, "msg_a"},
		{"anthropic stream message", `{"message":{"id":"msg_b","model":"m"}}`, false, "msg_b"},
		{"message outranks bare id", `{"id":"block","message":{"id":"msg_c"}}`, false, "msg_c"},
		{"responses whole", `{"id":"resp_a","model":"m"}`, false, "resp_a"},
		{"responses nested", `{"response":{"id":"resp_b","model":"m"}}`, false, "resp_b"},
		{"responses explicit", `{"response_id":"resp_c"}`, false, "resp_c"},
		{"gemini responseId", `{"responseId":"gem_a"}`, false, "gem_a"},
		{"gemini nested responseId", `{"response":{"responseId":"gem_b"}}`, false, "gem_b"},
		{"response id outranks item id", `{"type":"response.output_item.added","id":"item_1","response":{"id":"resp_win"}}`, false, "resp_win"},
		{"request top-level id ignored", `{"id":"req-id","message":{"id":"req-msg"},"model":"m"}`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan := scanProviderJSON
			if tc.request {
				scan = scanProviderJSONRequest
			}
			values, err := scan(strings.NewReader(tc.body), int64(len(tc.body))+1)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if values.ResponseID != tc.want {
				t.Fatalf("response id = %q, want %q", values.ResponseID, tc.want)
			}
		})
	}
}

func TestSupplierStreamReplyID(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		format providerStreamFormat
		want   string
	}{
		{"chat SSE", "data: {\"id\":\"chatcmpl-a\",\"model\":\"m\"}\n\n", providerStreamSSE, "chatcmpl-a"},
		{"responses SSE", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\",\"model\":\"m\"}}\n\n", providerStreamSSE, "resp_a"},
		{"anthropic SSE", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"m\"}}\n\n", providerStreamSSE, "msg_a"},
		{"gemini SSE", "data: {\"responseId\":\"gem_a\",\"modelVersion\":\"m\"}\n\n", providerStreamSSE, "gem_a"},
		{"multiline data", "data: {\"type\":\"response.created\",\ndata: \"response\":{\"id\":\"resp_m\"}}\n\n", providerStreamSSE, "resp_m"},
		{"NDJSON", "{\"id\":\"chatcmpl-n\",\"model\":\"m\"}\n", providerStreamNDJSON, "chatcmpl-n"},
		{"incomplete event ignored", "data: {\"id\":\"chatcmpl-x\"", providerStreamSSE, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supplierStreamReplyID([]byte(tc.body), tc.format); got != tc.want {
				t.Fatalf("reply id = %q, want %q", got, tc.want)
			}
		})
	}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestProviderJSONScannerSurvivesRefills(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"vendor/a","text":"\uD800"}`, "vendor/a"},
		{`{"text":"\uD800\u0041","model":"vendor/b"}`, "vendor/b"},
		{`{"model":"vendor/c","n":123.45e-6}`, "vendor/c"},
		{`{"model":"a\u002fb","n":0}`, "a/b"},
	}
	for _, tc := range cases {
		values, err := scanProviderJSON(&oneByteReader{data: []byte(tc.body)}, int64(len(tc.body))+1)
		if err != nil {
			t.Fatalf("scan %s: %v", tc.body, err)
		}
		if values.Model != tc.want {
			t.Fatalf("model %s=%q, want %q", tc.body, values.Model, tc.want)
		}
	}
}

// The backend address persists independently of response-name judgment, so
// these checks survive the removed provider mode.
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
