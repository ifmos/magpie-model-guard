package check

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type providerEnv struct {
	svc    *Service
	store  *Store
	native *httptest.Server
	guard  *httptest.Server
	rec    *nativeRecorder
}

func newProviderEnv(t *testing.T, handler nativeHandler) *providerEnv {
	t.Helper()
	svc, store := newRuleTestService(t)
	rec := &nativeRecorder{}
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(r, body)
		handler(w, r, body)
	}))
	t.Cleanup(native.Close)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = native.URL
	cfg.GatewayKey = "gw-key"
	cfg.ProviderEnabled = true
	cfg.ProviderKey = "provider-key"
	cfg.ProviderAttempts = 2
	if err := svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	guard := httptest.NewServer(registerProvider(http.NewServeMux(), svc))
	t.Cleanup(guard.Close)
	return &providerEnv{svc: svc, store: store, native: native, guard: guard, rec: rec}
}

func jsonOK(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func providerCall(t *testing.T, env *providerEnv, method, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, env.guard.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if req.Header.Get("Authorization") == "" && req.Header.Get("X-Goog-Api-Key") == "" {
		if !strings.Contains(path, "key=provider-key") {
			req.Header.Set("Authorization", "Bearer provider-key")
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func providerChatRequest(t *testing.T, url, key string, stream bool) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"model":"vendor/big","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	req, err := http.NewRequest(http.MethodPost, url+providerChatPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

type fakeProviderUpstream struct {
	models []string
	stream bool
	calls  atomic.Int32
	t      *testing.T
}

func (f *fakeProviderUpstream) handler(w http.ResponseWriter, r *http.Request, _ []byte) {
	if r.URL.Path == "/v1/models" {
		jsonOK(w, `{"object":"list","data":[{"id":"vendor/big","object":"model"}]}`)
		return
	}
	i := int(f.calls.Add(1)) - 1
	model := f.models[len(f.models)-1]
	if i < len(f.models) {
		model = f.models[i]
	}
	if f.stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", model)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": model,
		"choices": []any{map[string]any{
			"message":       map[string]string{"content": "ok"},
			"finish_reason": "stop",
		}},
	})
}

func newProviderTestService(t *testing.T, upstream *fakeProviderUpstream, attempts int) (*Service, *Store, *httptest.Server) {
	t.Helper()
	env := newProviderEnv(t, upstream.handler)
	env.svc.mu.Lock()
	_ = env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true, IntervalMin: 30,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}},
	})
	env.svc.mu.Unlock()
	cfg := mustConfig(t, env.store)
	cfg.ProviderAttempts = attempts
	if err := env.svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return env.svc, env.store, env.guard
}

// routeCase exercises the native wire mapping for one allowlisted endpoint.
type routeCase struct {
	clientPath string
	nativePath string
	method     string
	body       string
	query      string
}

func TestProviderRoutesForwardNatively(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"ok":true}`)
	})
	cases := []routeCase{
		{providerChatPath, "/v1/chat/completions", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/chat/completions", "/chat/completions", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/v1/responses", "/v1/responses", http.MethodPost, `{"model":"vendor/ok","input":"hi"}`, ""},
		{"/provider/backend-api/codex/responses", "/backend-api/codex/responses", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/v1/messages", "/v1/messages", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/v1beta/models/gemini-2.5-flash:generateContent", "/v1beta/models/gemini-2.5-flash:generateContent", http.MethodPost, `{"contents":[]}`, ""},
		{"/provider/v1beta/models/google%2Fgemini:streamGenerateContent", "/v1beta/models/google%2Fgemini:streamGenerateContent", http.MethodPost, `{"contents":[]}`, "alt=sse"},
		{"/provider/v1beta/models/google/gemini:generateContent", "/v1beta/models/google/gemini:generateContent", http.MethodPost, `{"contents":[]}`, ""},
		{"/provider/v1/messages/count_tokens", "/v1/messages/count_tokens", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/v1beta/models/gemini-2.5-flash:countTokens", "/v1beta/models/gemini-2.5-flash:countTokens", http.MethodPost, `{"contents":[]}`, ""},
		{"/provider/v1/systemone", "/v1/systemone", http.MethodPost, `{}`, ""},
		{"/provider/v1/alpha/search", "/v1/alpha/search", http.MethodPost, `{}`, ""},
		{"/provider/alpha/search", "/alpha/search", http.MethodPost, `{}`, ""},
		{"/provider/v1/images/generations", "/v1/images/generations", http.MethodPost, `{}`, ""},
		{"/provider/v1/images/edits", "/v1/images/edits", http.MethodPost, "raw-bytes", ""},
		{"/provider/v1/embeddings", "/v1/embeddings", http.MethodPost, `{}`, ""},
		{"/provider/v1/rerank", "/v1/rerank", http.MethodPost, `{}`, ""},
		{"/provider/v1/audio/transcriptions", "/v1/audio/transcriptions", http.MethodPost, "audio", ""},
		{"/provider/v1/audio/translations", "/v1/audio/translations", http.MethodPost, "audio", ""},
		{"/provider/v1/audio/speech", "/v1/audio/speech", http.MethodPost, `{}`, ""},
		{"/provider/audio/speech", "/audio/speech", http.MethodPost, `{}`, ""},
		{"/provider/v1/videos", "/v1/videos", http.MethodPost, `{}`, ""},
		{"/provider/videos", "/videos", http.MethodPost, `{}`, ""},
		{"/provider/v1/videos/vid_123", "/v1/videos/vid_123", http.MethodGet, "", ""},
		{"/provider/v1/videos/vid_123/content", "/v1/videos/vid_123/content", http.MethodGet, "", ""},
		{"/provider/videos/vid_123/content", "/videos/vid_123/content", http.MethodGet, "", ""},
		{"/provider/", "/", http.MethodGet, "", ""},
		{"/provider/v1", "/v1", http.MethodGet, "", ""},
		{"/provider/v1/", "/v1/", http.MethodGet, "", ""},
		{"/provider/v1/models", "/v1/models", http.MethodGet, "", ""},
		{"/provider/models", "/models", http.MethodGet, "", ""},
		{"/provider/v1/models/vendor/extra", "/v1/models/vendor/extra", http.MethodGet, "", ""},
		{"/provider/v1beta/models", "/v1beta/models", http.MethodGet, "", ""},
		{"/provider/v1/codex/models", "/v1/codex/models", http.MethodGet, "", ""},
		{"/provider/backend-api/codex/models", "/backend-api/codex/models", http.MethodGet, "", ""},
		{"/provider/muse-code/models", "/muse-code/models", http.MethodGet, "", ""},
		{"/provider/api/hello", "/api/hello", http.MethodGet, "", ""},
		{"/provider/api/version", "/api/version", http.MethodGet, "", ""},
		{"/provider/api/tags", "/api/tags", http.MethodGet, "", ""},
		{"/provider/api/show", "/api/show", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/api/chat", "/api/chat", http.MethodPost, `{"model":"vendor/ok","messages":[]}`, ""},
		{"/provider/api/generate", "/api/generate", http.MethodPost, `{"model":"vendor/ok","prompt":"hi"}`, ""},
		{"/provider/backend-api/codex/responses/compact", "/backend-api/codex/responses/compact", http.MethodPost, `{"model":"vendor/ok"}`, ""},
		{"/provider/backend-api/codex/alpha/search", "/backend-api/codex/alpha/search", http.MethodPost, `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.clientPath, "/", "_"), func(t *testing.T) {
			url := tc.clientPath
			if tc.query != "" {
				url += "?" + tc.query
			}
			resp := providerCall(t, env, tc.method, url, []byte(tc.body), map[string]string{
				"Content-Type":        "application/json",
				"Cookie":              "mg_session=secret",
				"Proxy-Authorization": "Basic x",
				"X-Forwarded-For":     "10.0.0.1",
				"Anthropic-Version":   "2023-06-01",
				"X-Magpie-Session":    "session-1",
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s status=%d", tc.method, url, resp.StatusCode)
			}
			got := env.rec.last()
			if got.method != tc.method || got.escaped != tc.nativePath {
				t.Fatalf("native got %s %s, want %s %s", got.method, got.escaped, tc.method, tc.nativePath)
			}
			if got.query != tc.query && !(tc.query == "" && got.query == "") {
				t.Fatalf("native query=%q, want %q", got.query, tc.query)
			}
			if got.header.Get("Authorization") != "Bearer gw-key" {
				t.Fatalf("native Authorization=%q", got.header.Get("Authorization"))
			}
			for _, removed := range []string{"Cookie", "Proxy-Authorization", "X-Forwarded-For"} {
				if value := got.header.Get(removed); value != "" {
					t.Fatalf("%s leaked upstream: %q", removed, value)
				}
			}
			if got.header.Get("Anthropic-Version") != "2023-06-01" || got.header.Get("X-Magpie-Session") != "session-1" {
				t.Fatalf("end-to-end headers dropped: %v", got.header)
			}
			if tc.method == http.MethodPost && got.body != nil && string(got.body) != tc.body {
				t.Fatalf("native body=%q, want %q", got.body, tc.body)
			}
			if strings.Contains(string(got.body), ProbeAliasPrefix) {
				t.Fatal("business request must keep its original model")
			}
		})
	}
}

func TestProviderModelQueryCredentialIsStripped(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"object":"list","data":[]}`)
	})
	resp := providerCall(t, env, http.MethodGet, "/provider/v1/models?format=json&key=provider-key", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got := env.rec.last()
	if got.query != "format=json" {
		t.Fatalf("native query=%q, want format=json", got.query)
	}
	if got.header.Get("Authorization") != "Bearer gw-key" {
		t.Fatalf("native auth=%q", got.header.Get("Authorization"))
	}
}

func TestProviderRejectsUnknownAndManagementPaths(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unknown path reached native: %s", r.URL.Path)
	})
	paths := []string{
		"/provider/v1/magpie/keys", "/provider/v1/magpie/restart", "/provider/v1/heap",
		"/provider/mcp/tools", "/provider/v1/chat%2Fcompletions", "/provider/v1/../admin",
		"/provider/api/../v1/magpie/keys",
	}
	for _, path := range paths {
		req, err := http.NewRequest(http.MethodGet, env.guard.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer provider-key")
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
		t.Fatalf("native calls=%d, want 0", got)
	}
}

func TestProviderMethodNotAllowed(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("wrong method reached native")
	})
	resp := providerCall(t, env, http.MethodGet, providerChatPath, nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, http.MethodPost) {
		t.Fatalf("Allow=%q", allow)
	}
	if env.rec.count() != 0 {
		t.Fatal("wrong method must not reach native")
	}
}

func TestProviderCatalogPassthrough(t *testing.T) {
	catalog := `{"object":"list","data":[{"id":"vendor/unknown","object":"model","context_window":128000,` +
		`"modalities":["text","image"],"reasoning_levels":["low","high"],"native_endpoints":{"chat":true},` +
		`"pricing":{"input":1},"service_tiers":["flex"],"future_field":{"nested":true}}],"has_more":false}`
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Extra", "keep")
		_, _ = io.WriteString(w, catalog)
	})
	resp := providerCall(t, env, http.MethodGet, providerModelsPath, nil, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != catalog {
		t.Fatalf("catalog altered:\n%s", body)
	}
	if resp.Header.Get("X-Extra") != "keep" {
		t.Fatal("end-to-end response header dropped")
	}
}

func TestProviderNativeErrorsPassThrough(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusNotFound} {
		env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
			w.Header().Set("Content-Type", "application/json")
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "7")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"native"}}`)
		})
		resp := providerCall(t, env, http.MethodGet, providerModelsPath, nil, nil)
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

func TestProviderUnmonitoredModelIsCallable(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/other"}`)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{Model: "vendor/big", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/other"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := env.rec.last().body; string(got) != `{"model":"vendor/other"}` {
		t.Fatalf("model rewritten: %s", got)
	}
}

func TestProviderBlockedTargetStopsBeforeUpstream(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("blocked target reached native")
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{Model: "vendor/big", Enabled: true, State: StateFailed}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("blocked target must not call upstream")
	}
}

func TestProviderProtocolsJudgeVendorModel(t *testing.T) {
	type protocolCase struct {
		name    string
		path    string
		request string
		respond func(w http.ResponseWriter, model string)
		want    string
	}
	cases := []protocolCase{
		{
			name: "chat", path: providerChatPath,
			request: `{"model":"vendor/big","messages":[]}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"model":%q,"choices":[{"message":{"content":"ok"}}]}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "responses", path: "/provider/v1/responses",
			request: `{"model":"vendor/big","input":"hi"}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"object":"response","response":{"model":%q}}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "anthropic", path: "/provider/v1/messages",
			request: `{"model":"vendor/big","messages":[]}`,
			respond: func(w http.ResponseWriter, model string) {
				jsonOK(w, fmt.Sprintf(`{"type":"message","message":{"model":%q}}`, model))
			},
			want: `"vendor/big"`,
		},
		{
			name: "gemini", path: "/provider/v1beta/models/vendor%2Fbig:generateContent",
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
			env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				calls.Add(1)
				if r.Header.Get("X-Magpie-Response-Model") != "vendor" {
					tc.respond(w, "vendor/big")
					return
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
			env.svc.mu.Lock()
			if err := env.store.SaveTarget(&Target{
				Model: "vendor/big", Enabled: true,
				ResponseRuleMode: ResponseRuleCustom,
				ResponseModelRules: []ResponseModelRule{{
					ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			env.svc.mu.Unlock()
			resp := providerCall(t, env, http.MethodPost, tc.path, []byte(tc.request), nil)
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

func TestProviderRecordDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		jsonOK(w, `{"model":"vendor/flash"}`)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-record", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
}

func TestProviderDisablesAfterAttempts(t *testing.T) {
	up := &fakeProviderUpstream{models: []string{"vendor/flash", "vendor/flash"}}
	_, store, ts := newProviderTestService(t, up, 2)

	resp := providerChatRequest(t, ts.URL, "provider-key", false)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "已尝试 2 次") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q", resp.Header.Get("Retry-After"))
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d, want 2", got)
	}
	target, _ := store.Target("vendor/big")
	if target == nil || target.State != StateFailed {
		t.Fatalf("target not disabled: %+v", target)
	}
}

func TestProviderRetriesStreaming(t *testing.T) {
	up := &fakeProviderUpstream{models: []string{"vendor/flash", "vendor/big"}, stream: true}
	_, _, ts := newProviderTestService(t, up, 2)

	resp := providerChatRequest(t, ts.URL, "provider-key", true)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"vendor/big"`) || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d, want 2", got)
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

func TestProviderGatewayBaseV1IsStripped(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"ok":true}`)
	})
	cfg := mustConfig(t, env.store)
	cfg.GatewayURL = env.native.URL + "/v1"
	cfg.GatewayKey = "gw-key"
	cfg.ProviderEnabled = true
	cfg.ProviderKey = "provider-key"
	if err := env.svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	resp := providerCall(t, env, http.MethodPost, "/provider/v1/chat/completions", []byte(`{"model":"vendor/ok"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := env.rec.last().path; got != "/v1/chat/completions" {
		t.Fatalf("native path=%q, want /v1/chat/completions", got)
	}
}

func TestProviderAuthAndModelList(t *testing.T) {
	up := &fakeProviderUpstream{models: []string{"vendor/big"}}
	_, _, ts := newProviderTestService(t, up, 2)

	resp, err := http.Get(ts.URL + providerModelsPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+providerModelsPath, nil)
	req.Header.Set("X-Api-Key", "provider-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(list.Data) != 1 || list.Data[0].ID != "vendor/big" {
		t.Fatalf("status=%d list=%+v", resp.StatusCode, list)
	}
}

func TestProviderCredentialConflicts(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("conflicting credentials reached native")
	})
	resp := providerCall(t, env, http.MethodGet, providerModelsPath, nil, map[string]string{
		"Authorization":  "Bearer provider-key",
		"X-Api-Key":      "other-key",
		"X-Goog-Api-Key": "provider-key",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("conflicting credentials must not reach native")
	}
}

func TestProviderDisabledReturnsNotFound(t *testing.T) {
	up := &fakeProviderUpstream{models: []string{"vendor/big"}}
	svc, store, ts := newProviderTestService(t, up, 2)
	cfg := mustConfig(t, store)
	cfg.ProviderEnabled = false
	if err := svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+providerModelsPath, nil)
	req.Header.Set("Authorization", "Bearer provider-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled provider status=%d, want 404", resp.StatusCode)
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
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unsupported encoding reached native")
	})
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), map[string]string{
		"Content-Encoding": "br",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("unsupported encoding must not reach native")
	}
}

func TestProviderBadJSON(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("bad JSON reached native")
	})
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestProviderBodyLimit(t *testing.T) {
	old := providerBodyLimit
	providerBodyLimit = 1024
	t.Cleanup(func() { providerBodyLimit = old })

	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("oversized body reached native")
	})
	body := []byte(`{"model":"vendor/big","pad":"` + strings.Repeat("a", 2048) + `"}`)
	resp := providerCall(t, env, http.MethodPost, providerChatPath, body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if env.rec.count() != 0 {
		t.Fatal("oversized body must not reach native")
	}
}

func TestProviderIdleTimeout(t *testing.T) {
	old := providerIdleRead
	providerIdleRead = 50 * time.Millisecond
	t.Cleanup(func() { providerIdleRead = old })

	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("timed out body reached native")
	})
	body := &testBody{data: []byte(`{"model":"vendor/big"`), block: true, closed: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/provider/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer provider-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.svc.apiProvider(rec, req)
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("status=%d, want 408", rec.Code)
	}
	if env.rec.count() != 0 {
		t.Fatal("timed out body must not reach native")
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
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
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
			resp := providerCall(t, env, http.MethodPost, providerChatPath, tc.body, map[string]string{
				"Content-Encoding": tc.encoding,
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			got := env.rec.last()
			if !bytes.Equal(got.body, tc.body) {
				t.Fatalf("upstream received re-encoded body")
			}
			if got.header.Get("Content-Encoding") != tc.encoding {
				t.Fatalf("Content-Encoding=%q", got.header.Get("Content-Encoding"))
			}
		})
	}
}

func TestProviderLargeBodyBeyond32MiB(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/big"}`)
	})
	pad := strings.Repeat("b", 33<<20)
	body := []byte(`{"model":"vendor/big","pad":"` + pad + `"}`)
	resp := providerCall(t, env, http.MethodPost, providerChatPath, body, nil)
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
			env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			})
			resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || string(body) != tc.stream {
				t.Fatalf("status=%d body=%q", resp.StatusCode, body)
			}
			if env.rec.count() != 1 {
				t.Fatalf("calls=%d, want 1 (no model or no rule must not retry)", env.rec.count())
			}
		})
	}
}

func TestProviderStreamOversizeEventPassesThrough(t *testing.T) {
	old := providerPrefixLimit
	providerPrefixLimit = 256
	t.Cleanup(func() { providerPrefixLimit = old })

	stream := "data: {\"model\":\"vendor/big\",\"pad\":\"" + strings.Repeat("x", 1024) + "\"}\n\n"
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	})
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"model\":\"vendor/big\",\"marker\":\"first\"}\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"model\":\"vendor/big\",\"marker\":\"second\"}\n\n")
		flusher.Flush()
		<-done
	}))
	defer func() {
		releaseUpstream()
		finishUpstream()
		upstream.Close()
	}()

	svc, store, ts := newProviderTestService(t, &fakeProviderUpstream{models: []string{"vendor/big"}}, 2)
	cfg := mustConfig(t, store)
	cfg.GatewayURL = upstream.URL
	if err := svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	resp := providerChatRequest(t, ts.URL, "provider-key", true)
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
			t.Fatalf("timed out waiting for %q before upstream EOF", marker)
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
		t.Fatal("upstream did not finish")
	}
}

func TestProviderActionFailureReturns500(t *testing.T) {
	var calls atomic.Int32
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		jsonOK(w, `{"model":"vendor/flash"}`)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	if _, err := env.store.db.Exec(`CREATE TRIGGER fail_results BEFORE INSERT ON results BEGIN SELECT RAISE(ABORT,'boom'); END`); err != nil {
		t.Fatal(err)
	}
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), "规则动作提交失败") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Fatal("action failure must not advertise Retry-After")
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State != StateFailed {
		t.Fatalf("persisted disable must be kept on action failure: %+v", target)
	}
	// The dedupe entry was not written, so a repaired guard can retry the action.
	target.State = StatePassed
	if err := env.store.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.db.Exec(`DROP TRIGGER fail_results`); err != nil {
		t.Fatal(err)
	}
	resp = providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("retry after repair status=%d body=%s", resp.StatusCode, body)
	}
	target, _ = env.store.Target("vendor/big")
	if target == nil || target.State != StateFailed {
		t.Fatalf("recovered submission must disable the target: %+v", target)
	}
}

func TestProviderRerouteFailurePreservesExistingKVs(t *testing.T) {
	var current atomic.Value
	current.Store("vendor/flash")
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, fmt.Sprintf(`{"model":%q}`, current.Load().(string)))
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-reroute", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionReroute,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
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
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
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
	current.Store("vendor/flash2")
	resp = providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
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
	if !strings.Contains(joined, "vendor/keep") || !strings.Contains(joined, "vendor/flash2") {
		t.Fatalf("reroutes=%v", routes)
	}
}

func TestProviderDecodesCompressedResponseOnlyWithRules(t *testing.T) {
	inner := []byte(`{"model":"vendor/big","choices":[]}`)
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Header.Get("Accept-Encoding") == "identity" {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gzipBytes(t, inner))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(gzipBytes(t, inner))
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-record", Match: ResponseMatchContains, Pattern: "none", Action: ResponseActionRecord,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, inner) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("decoded delivery kept Content-Encoding=%q", resp.Header.Get("Content-Encoding"))
	}
}

func TestProviderTransparentPathKeepsCompression(t *testing.T) {
	inner := []byte(`{"model":"vendor/big","choices":[]}`)
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(gzipBytes(t, inner))
	})
	req, _ := http.NewRequest(http.MethodPost, env.guard.URL+providerChatPath, bytes.NewReader([]byte(`{"model":"vendor/big"}`)))
	req.Header.Set("Authorization", "Bearer provider-key")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(raw, gzipBytes(t, inner)) {
		t.Fatalf("transparent response altered: encoding=%q len=%d", resp.Header.Get("Content-Encoding"), len(raw))
	}
}

func TestProviderHeadSemantics(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"object":"list","data":[]}`)
	})
	resp := providerCall(t, env, http.MethodHead, providerModelsPath, nil, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if got := env.rec.last(); got.method != http.MethodHead {
		t.Fatalf("native method=%s", got.method)
	}
}

func TestProviderCORSPreflight(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodOptions {
			t.Errorf("preflight reached a non-OPTIONS handler")
		}
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.WriteHeader(http.StatusNoContent)
	})
	req, _ := http.NewRequest(http.MethodOptions, env.guard.URL+providerChatPath, nil)
	req.Header.Set("Origin", "https://allowed.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://allowed.example" {
		t.Fatalf("preflight status=%d headers=%v", resp.StatusCode, resp.Header)
	}
	if env.rec.count() != 1 {
		t.Fatalf("preflight calls=%d, want 1", env.rec.count())
	}

	// Unknown paths cannot be probed through OPTIONS.
	req, _ = http.NewRequest(http.MethodOptions, env.guard.URL+"/provider/v1/magpie/keys", nil)
	req.Header.Set("Origin", "https://allowed.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown preflight status=%d", resp.StatusCode)
	}
	// A method that the route does not serve is rejected before the gateway.
	req, _ = http.NewRequest(http.MethodOptions, env.guard.URL+providerChatPath, nil)
	req.Header.Set("Origin", "https://allowed.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("bad preflight method status=%d", resp.StatusCode)
	}
	if env.rec.count() != 1 {
		t.Fatalf("rejected preflights reached native: calls=%d", env.rec.count())
	}
}

func TestProviderOllamaNeverJudgesResponseModel(t *testing.T) {
	stream := `{"model":"vendor/bad","message":{"role":"assistant","content":"ok"},"done":false}` + "\n" +
		`{"model":"vendor/bad","done":true}` + "\n"
	var calls atomic.Int32
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, stream)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/bad", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-bad", Match: ResponseMatchContains, Pattern: "vendor/bad", Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	resp := providerCall(t, env, http.MethodPost, "/provider/api/chat",
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

func TestProviderUnmonitoredRequestUsesOnlyGlobalRules(t *testing.T) {
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		jsonOK(w, `{"model":"vendor/flash"}`)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/monitored", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-other", Match: ResponseMatchContains, Pattern: "xyz", Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	cfg := mustConfig(t, env.store)
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{
		ID: "r-global", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}}
	if err := env.svc.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	resp := providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/unmonitored"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	monitored, _ := env.store.Target("vendor/monitored")
	if monitored == nil || monitored.State == StateFailed {
		t.Fatalf("a different monitored target must not be disabled: %+v", monitored)
	}
	results, err := env.store.Results("vendor/unmonitored", "response-model", 10)
	if err != nil || len(results) == 0 {
		t.Fatalf("results=%v err=%v", results, err)
	}
	if !strings.Contains(results[0].Detail, "未找到监测目标，未禁用") || results[0].RuleID != "r-global" {
		t.Fatalf("result=%+v", results[0])
	}
}

func TestProviderKeepsRuleSnapshotForInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	env := newProviderEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		jsonOK(w, `{"model":"vendor/flash"}`)
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-record", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionRecord,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()

	done := make(chan *http.Response, 1)
	go func() {
		done <- providerCall(t, env, http.MethodPost, providerChatPath, []byte(`{"model":"vendor/big"}`), nil)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach native")
	}
	// The rule changes while the request is in flight; the snapshot must win.
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-disable", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
		}},
	}); err != nil {
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
	results, err := env.store.Results("vendor/big", "response-model", 10)
	if err != nil || len(results) == 0 || results[0].RuleID != "r-record" {
		t.Fatalf("results=%v err=%v", results, err)
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
