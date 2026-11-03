package check

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	providerModelsPath = "/provider/v1/models"
	providerChatPath   = "/provider/v1/chat/completions"
)

// Limits are variables so tests can shrink them without allocating hundreds of
// megabytes; production values follow the native gateway.
var (
	providerBodyLimit int64 = 512 << 20
	providerIdleRead        = 60 * time.Second
)

// providerProtocol selects the error envelope used for locally generated
// failures. Native upstream errors are always passed through untouched.
type providerProtocol string

const (
	providerProtoChat      providerProtocol = "chat"
	providerProtoResponses providerProtocol = "responses"
	providerProtoAnthropic providerProtocol = "anthropic"
	providerProtoGemini    providerProtocol = "gemini"
	providerProtoOllama    providerProtocol = "ollama"
)

// providerEndpoint describes one native gateway path. gate extracts the
// request model and enforces the target block state; judge applies response
// rules with bounded retries.
type providerEndpoint struct {
	methods   []string
	proto     providerProtocol
	gate      bool
	judge     bool
	modelPath bool
	ws        bool
}

// providerUpstreamTransport keeps client encoding negotiation intact and never
// lets the transport decode bodies behind the guard's back.
var providerUpstreamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	t.MaxIdleConns = 100
	t.IdleConnTimeout = 90 * time.Second
	return t
}()

// providerFixedRoutes is the explicit allowlist of native paths. Management,
// key issuance, MCP and debug endpoints are intentionally absent.
var providerFixedRoutes = buildProviderRoutes()

func addProviderRoutes(routes map[string]providerEndpoint, methods []string, proto providerProtocol, gate, judge bool, paths ...string) {
	for _, path := range paths {
		routes[path] = providerEndpoint{methods: methods, proto: proto, gate: gate, judge: judge}
	}
}

func buildProviderRoutes() map[string]providerEndpoint {
	routes := map[string]providerEndpoint{}
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}
	postGet := []string{http.MethodPost, http.MethodGet}

	// Catalog and model metadata.
	addProviderRoutes(routes, get, providerProtoChat, false, false,
		"/", "/v1", "/v1/", "/v1/models", "/models", "/v1beta/models",
		"/v1/codex/models", "/muse-code/models", "/backend-api/codex/models")

	// Conversations.
	addProviderRoutes(routes, post, providerProtoChat, true, true,
		"/v1/chat/completions", "/chat/completions")
	addProviderRoutes(routes, postGet, providerProtoResponses, true, true,
		"/v1/responses", "/responses", "/backend-api/codex/responses")
	addProviderRoutes(routes, post, providerProtoAnthropic, true, true,
		"/v1/messages", "/messages")
	addProviderRoutes(routes, post, providerProtoResponses, false, false,
		"/v1/responses/compact", "/responses/compact",
		"/backend-api/codex/responses/compact")

	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		ep := routes[path]
		ep.ws = true
		routes[path] = ep
	}

	// Auxiliary generation endpoints are proxy-only.
	addProviderRoutes(routes, post, providerProtoAnthropic, false, false,
		"/v1/messages/count_tokens")
	addProviderRoutes(routes, post, providerProtoResponses, false, false,
		"/v1/systemone", "/v1/alpha/search", "/alpha/search",
		"/backend-api/codex/alpha/search")

	// Media, retrieval, audio and video creation.
	addProviderRoutes(routes, post, providerProtoChat, false, false,
		"/v1/images/generations", "/images/generations",
		"/v1/images/edits", "/images/edits",
		"/v1/embeddings", "/embeddings",
		"/v1/rerank", "/rerank",
		"/v1/audio/transcriptions", "/audio/transcriptions",
		"/v1/audio/translations", "/audio/translations",
		"/v1/audio/speech", "/audio/speech",
		"/v1/videos", "/videos")

	// Ollama compatibility.
	addProviderRoutes(routes, get, providerProtoOllama, false, false,
		"/api/hello", "/api/version", "/api/tags")
	addProviderRoutes(routes, post, providerProtoOllama, false, false, "/api/show")
	addProviderRoutes(routes, post, providerProtoOllama, true, false,
		"/api/chat", "/api/generate")

	return routes
}

// providerPrefixOwned reports whether a request belongs to the provider.
func providerPrefixOwned(path string) bool {
	return path == "/provider" || strings.HasPrefix(path, "/provider/")
}

// registerProvider composes the provider prefix ahead of the guard mux. A
// plain "/provider/" pattern would conflict with the console catch-all
// ("GET /"), so the prefix is dispatched here instead of being registered.
func registerProvider(mux *http.ServeMux, s *Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upstream" || strings.HasPrefix(r.URL.Path, "/upstream/") {
			s.apiSupplier(w, r)
			return
		}
		if providerPrefixOwned(r.URL.Path) {
			s.apiProvider(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// providerPathParts splits the path after the /provider prefix. Escaped
// segments are preserved for the upstream URL while decoded segments are used
// for routing, so an encoded slash cannot masquerade as a fixed path segment.
type providerPathParts struct {
	escapedRest string
	segments    []providerPathSegment
}

type providerPathSegment struct {
	escaped string
	decoded string
}

func parseProviderPath(u *url.URL) (providerPathParts, bool) {
	escaped := u.EscapedPath()
	if escaped == "/provider" {
		return providerPathParts{escapedRest: "/"}, true
	}
	if !strings.HasPrefix(escaped, "/provider/") {
		return providerPathParts{}, false
	}
	rest := escaped[len("/provider"):]
	parts := providerPathParts{escapedRest: rest}
	tail := strings.TrimPrefix(rest, "/")
	if tail == "" {
		return parts, true
	}
	for _, raw := range strings.Split(tail, "/") {
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			return providerPathParts{}, false
		}
		if decoded == "." || decoded == ".." || strings.ContainsRune(decoded, '\x00') {
			return providerPathParts{}, false
		}
		parts.segments = append(parts.segments, providerPathSegment{escaped: raw, decoded: decoded})
	}
	return parts, true
}

type providerMatch struct {
	endpoint  providerEndpoint
	known     bool
	allowed   bool
	allow     string
	pathModel string
}

func providerAllowHeader(methods []string) string {
	out := make([]string, 0, len(methods)+1)
	for _, m := range methods {
		out = append(out, m)
		if m == http.MethodGet {
			out = append(out, http.MethodHead)
		}
	}
	return strings.Join(out, ", ")
}

func providerMethodAllowed(methods []string, method string) bool {
	for _, m := range methods {
		if m == method || (method == http.MethodHead && m == http.MethodGet) {
			return true
		}
	}
	return false
}

func matchProviderEndpoint(method string, parts providerPathParts) providerMatch {
	match := func(ep providerEndpoint, pathModel string) providerMatch {
		return providerMatch{
			endpoint:  ep,
			known:     true,
			allowed:   providerMethodAllowed(ep.methods, method),
			allow:     providerAllowHeader(ep.methods),
			pathModel: pathModel,
		}
	}
	if len(parts.segments) == 0 {
		return match(providerFixedRoutes["/"], "")
	}
	// Fixed paths only match when every segment is a plain segment; an encoded
	// slash inside a segment must not recombine into a fixed route.
	fixed := true
	for _, seg := range parts.segments {
		if strings.Contains(seg.decoded, "/") {
			fixed = false
			break
		}
	}
	if fixed {
		key := "/" + strings.Join(providerSegmentValues(parts.segments), "/")
		if ep, ok := providerFixedRoutes[key]; ok {
			return match(ep, "")
		}
	}
	// GET /v1/models/{id...}: model ids may contain path separators.
	if len(parts.segments) >= 3 && parts.segments[0].decoded == "v1" && parts.segments[1].decoded == "models" {
		return match(providerEndpoint{methods: []string{http.MethodGet}, proto: providerProtoChat}, "")
	}
	// Gemini generate and count tokens; the model may contain slashes and colons.
	if len(parts.segments) >= 3 && parts.segments[0].decoded == "v1beta" && parts.segments[1].decoded == "models" {
		last := parts.segments[len(parts.segments)-1].decoded
		idx := strings.LastIndex(last, ":")
		if idx > 0 {
			modelSegments := make([]string, 0, len(parts.segments)-2)
			for _, seg := range parts.segments[2 : len(parts.segments)-1] {
				if seg.decoded == "" {
					idx = -1
					break
				}
				modelSegments = append(modelSegments, seg.decoded)
			}
			if idx > 0 && last[:idx] != "" {
				modelSegments = append(modelSegments, last[:idx])
			} else {
				idx = -1
			}
			if idx > 0 {
				model := strings.Join(modelSegments, "/")
				switch last[idx+1:] {
				case "generateContent", "streamGenerateContent":
					return match(providerEndpoint{methods: []string{http.MethodPost}, proto: providerProtoGemini, gate: true, judge: true, modelPath: true}, model)
				case "countTokens":
					return match(providerEndpoint{methods: []string{http.MethodPost}, proto: providerProtoGemini}, "")
				}
			}
		}
		return providerMatch{endpoint: providerEndpoint{methods: []string{http.MethodPost}, proto: providerProtoGemini}, known: true, allowed: false, allow: http.MethodPost}
	}
	// Video polling and content; the id is one opaque escaped segment.
	for _, base := range []int{0, 1} {
		if len(parts.segments) != base+2 && len(parts.segments) != base+3 {
			continue
		}
		if base == 1 && parts.segments[0].decoded != "v1" {
			continue
		}
		if parts.segments[base].decoded != "videos" {
			continue
		}
		if len(parts.segments) == base+3 && parts.segments[base+2].decoded != "content" {
			continue
		}
		return match(providerEndpoint{methods: []string{http.MethodGet}, proto: providerProtoChat}, "")
	}
	return providerMatch{}
}

func providerSegmentValues(segments []providerPathSegment) []string {
	out := make([]string, len(segments))
	for i, seg := range segments {
		out[i] = seg.decoded
	}
	return out
}

func guessProviderProtocol(path string) providerProtocol {
	lower := strings.ToLower(path)
	switch {
	case strings.Contains(lower, "/v1beta/"):
		return providerProtoGemini
	case strings.Contains(lower, "/messages"):
		return providerProtoAnthropic
	case strings.Contains(lower, "/api/"):
		return providerProtoOllama
	case strings.Contains(lower, "/backend-api/") || strings.Contains(lower, "/responses"):
		return providerProtoResponses
	}
	return providerProtoChat
}

// apiProvider authenticates, classifies and proxies one provider request.
func (s *Service) apiProvider(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.LoadConfig()
	if err != nil {
		writeProviderError(w, providerProtoChat, http.StatusInternalServerError, "读取 Provider 配置失败", false)
		return
	}
	if !cfg.ProviderEnabled {
		http.NotFound(w, r)
		return
	}
	parts, ok := parseProviderPath(r.URL)
	if !ok {
		writeProviderError(w, guessProviderProtocol(r.URL.Path), http.StatusNotFound, "未知的 Provider 路径", false)
		return
	}
	matched := matchProviderEndpoint(r.Method, parts)
	proto := matched.endpoint.proto
	if proto == "" {
		proto = guessProviderProtocol(parts.escapedRest)
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		s.providerPreflight(w, r, cfg, parts, matched)
		return
	}
	if !s.providerAuthorized(w, r, cfg, proto) {
		return
	}
	if !matched.known {
		writeProviderError(w, proto, http.StatusNotFound, "未知的 Provider 路径", false)
		return
	}
	if !matched.allowed {
		w.Header().Set("Allow", matched.allow)
		writeProviderError(w, proto, http.StatusMethodNotAllowed, "请求方法不允许", false)
		return
	}
	if ValidateGateway(cfg.GatewayURL) != nil || strings.TrimSpace(cfg.GatewayKey) == "" {
		writeProviderError(w, proto, http.StatusServiceUnavailable, "Magpie 网关尚未配置", false)
		return
	}
	if matched.endpoint.ws && r.Method == http.MethodGet && isProviderUpgrade(r) {
		s.serveProviderSocket(w, r, cfg, matched)
		return
	}
	if !matched.endpoint.gate || r.Method != http.MethodPost {
		s.proxyProvider(w, r, cfg, matched, nil)
		return
	}

	spool, values, status, message := s.spoolProviderRequest(w, r)
	if status != 0 {
		writeProviderError(w, proto, status, message, false)
		return
	}
	defer spool.Close()

	requested := strings.TrimSpace(values.Model)
	if matched.endpoint.modelPath {
		requested = strings.TrimSpace(matched.pathModel)
	}
	targets, err := s.store.Targets()
	if err != nil {
		writeProviderError(w, proto, http.StatusInternalServerError, "读取监测目标失败", false)
		return
	}
	target := findTargetForRequest(targets, requested)
	if target != nil && target.blockedAt(time.Now()) {
		writeProviderError(w, proto, http.StatusForbidden, "该模型已被守卫禁用", false)
		return
	}
	rules := effectiveResponseRules(cfg, target)
	if !matched.endpoint.judge || !hasActiveResponseRules(rules) {
		rules = nil
	}
	if values.Compaction {
		// Native compaction is a single-shot link; retrying it would replay it.
		rules = nil
	}
	attempts := cfg.ProviderAttempts
	if attempts < 1 || attempts > 5 {
		attempts = 2
	}
	state := &providerProxyState{
		svc:       s,
		cfg:       cfg,
		proto:     proto,
		spool:     spool,
		requested: requested,
		target:    target,
		rules:     rules,
		attempts:  attempts,
	}
	s.proxyProvider(w, r, cfg, matched, state)
}

// providerPreflight forwards CORS preflight for allowlisted paths only. The
// native gateway decides the origin policy; the guard adds no allowlist.
func (s *Service) providerPreflight(w http.ResponseWriter, r *http.Request, cfg GuardConfig, parts providerPathParts, matched providerMatch) {
	requestedMethod := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	if !matched.known {
		writeProviderError(w, guessProviderProtocol(parts.escapedRest), http.StatusNotFound, "未知的 Provider 路径", false)
		return
	}
	if !providerMethodAllowed(matched.endpoint.methods, requestedMethod) {
		w.Header().Set("Allow", matched.allow)
		writeProviderError(w, matched.endpoint.proto, http.StatusMethodNotAllowed, "预检请求方法不在允许范围", false)
		return
	}
	if ValidateGateway(cfg.GatewayURL) != nil || strings.TrimSpace(cfg.GatewayKey) == "" {
		writeProviderError(w, matched.endpoint.proto, http.StatusServiceUnavailable, "Magpie 网关尚未配置", false)
		return
	}
	s.proxyProvider(w, r, cfg, matched, nil)
}

func hasActiveResponseRules(rules []ResponseModelRule) bool {
	for _, rule := range rules {
		if !rule.Disabled {
			return true
		}
	}
	return false
}

// providerAuthorized admits exactly one non-empty credential from the
// supported header/query locations. Conflicting credentials are rejected
// instead of guessing a precedence; keyless clients (Ollama) are not exempt.
func (s *Service) providerAuthorized(w http.ResponseWriter, r *http.Request, cfg GuardConfig, proto providerProtocol) bool {
	token, ok := providerRequestToken(r)
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-guard"`)
		writeProviderError(w, proto, http.StatusUnauthorized, "缺少有效的 Provider key", false)
		return false
	}
	if cfg.ProviderKey == "" || subtle.ConstantTimeCompare([]byte(token), []byte(cfg.ProviderKey)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-guard"`)
		writeProviderError(w, proto, http.StatusUnauthorized, "Provider key 无效", false)
		return false
	}
	return true
}

func providerRequestToken(r *http.Request) (string, bool) {
	var tokens []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range tokens {
			if existing == value {
				return
			}
		}
		tokens = append(tokens, value)
	}
	if raw := strings.TrimSpace(r.Header.Get("Authorization")); raw != "" {
		if !strings.HasPrefix(strings.ToLower(raw), "bearer ") {
			return "", false
		}
		add(raw[len("Bearer "):])
	}
	for _, key := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key"} {
		for _, value := range r.Header.Values(key) {
			add(value)
		}
	}
	add(r.URL.Query().Get("key"))
	if len(tokens) != 1 {
		return "", false
	}
	return tokens[0], true
}

// providerProxyState carries one judged request through its attempts.
type providerProxyState struct {
	svc       *Service
	cfg       GuardConfig
	proto     providerProtocol
	spool     *providerSpool
	requested string
	target    *Target
	rules     []ResponseModelRule
	attempts  int
	supplier  *supplierRequest
}

// proxyProvider wires the standard reverse proxy for one request. Judged
// requests get a retrying transport; everything else streams untouched.
func (s *Service) proxyProvider(w http.ResponseWriter, r *http.Request, cfg GuardConfig, matched providerMatch, state *providerProxyState) {
	target, err := s.providerUpstreamTarget(cfg, r)
	if err != nil {
		var loop *providerLoopError
		if errors.As(err, &loop) {
			writeProviderError(w, matched.endpoint.proto, http.StatusBadGateway,
				"上游网关指向 Provider 自身，请修正 Magpie 网关地址以避免请求环", false)
			return
		}
		writeProviderError(w, matched.endpoint.proto, http.StatusBadGateway, "上游网关地址无效："+err.Error(), false)
		return
	}
	var rt http.RoundTripper = providerUpstreamTransport
	if state != nil {
		rt = &providerTransport{base: providerUpstreamTransport, state: state}
	}
	proto := matched.endpoint.proto
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rewriteProviderRequest(pr, cfg, target, state)
		},
		Transport:     rt,
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			var local *providerLocalError
			if errors.As(err, &local) {
				writeProviderError(rw, proto, local.status, local.message, local.retryAfter)
				return
			}
			if req.Context().Err() != nil {
				return
			}
			writeProviderError(rw, proto, http.StatusBadGateway, "上游模型请求失败："+err.Error(), false)
		},
	}
	proxy.ServeHTTP(w, r)
}

func rewriteProviderRequest(pr *httputil.ProxyRequest, cfg GuardConfig, target *url.URL, state *providerProxyState) {
	pr.Out.URL = target
	pr.Out.Host = target.Host
	stripProviderClientHeaders(pr.Out.Header)
	pr.Out.Header.Set("Authorization", "Bearer "+cfg.GatewayKey)
	if state != nil && len(state.rules) > 0 {
		// Magpie must report the vendor model; its default reply naming would
		// replace it with the requested model and defeat the judgment.
		pr.Out.Header.Set("X-Magpie-Response-Model", "vendor")
		pr.Out.Header.Set("Accept-Encoding", "identity")
	}
	if state != nil {
		pr.Out.Body = nil
		pr.Out.GetBody = nil
		pr.Out.ContentLength = state.spool.Size()
	}
}

// stripProviderClientHeaders removes every client credential plus forwarding
// headers. Magpie must only see the guard gateway key.
func stripProviderClientHeaders(h http.Header) {
	for _, key := range []string{
		"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key",
		"Cookie", "Proxy-Authorization", "Proxy-Authenticate",
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-Ip", "Content-Length",
	} {
		h.Del(key)
	}
}

// providerLoopError marks an upstream configuration that points back at the
// guard's own provider prefix.
type providerLoopError struct{}

func (*providerLoopError) Error() string { return "provider loop" }

func sameProviderHost(a, b string) bool {
	ah, ap, err := net.SplitHostPort(a)
	if err != nil {
		return false
	}
	bh, bp, err := net.SplitHostPort(b)
	if err != nil || ap != bp {
		return false
	}
	normalize := func(host string) string {
		host = strings.ToLower(strings.Trim(host, "[]"))
		switch host {
		case "127.0.0.1", "::1", "localhost":
			return "loopback"
		}
		return host
	}
	return normalize(ah) == normalize(bh)
}

// providerUpstreamTarget builds the native URL from the configured gateway
// root plus the escaped path after /provider. Model ids keep their exact
// escapes; only the guard credential query parameter is removed.
func (s *Service) providerUpstreamTarget(cfg GuardConfig, r *http.Request) (*url.URL, error) {
	root, err := url.Parse(strings.TrimSpace(cfg.GatewayURL))
	if err != nil || (root.Scheme != "http" && root.Scheme != "https") || root.Host == "" || root.User != nil {
		return nil, errors.New("gateway url is invalid")
	}
	parts, ok := parseProviderPath(r.URL)
	if !ok {
		return nil, errors.New("path is invalid")
	}
	base := strings.TrimSuffix(root.EscapedPath(), "/")
	base = strings.TrimSuffix(base, "/v1")
	escapedRest := parts.escapedRest
	if escapedRest == "" {
		escapedRest = "/"
	}
	joined := base + escapedRest
	decoded, err := url.PathUnescape(joined)
	if err != nil {
		return nil, err
	}
	out := *root
	out.Path = decoded
	if decoded != joined {
		out.RawPath = joined
	} else {
		out.RawPath = ""
	}
	out.RawQuery = stripProviderQueryKey(r.URL.RawQuery)
	out.Fragment = ""
	out.User = nil
	if s.providerOrigin != "" {
		if origin, err := url.Parse(s.providerOrigin); err == nil && strings.EqualFold(origin.Scheme, out.Scheme) && sameProviderHost(origin.Host, out.Host) {
			if out.Path == "/provider" || strings.HasPrefix(out.Path, "/provider/") {
				return nil, &providerLoopError{}
			}
		}
	}
	return &out, nil
}

func stripProviderQueryKey(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, part := range parts {
		if part == "" {
			continue
		}
		name := part
		if idx := strings.IndexByte(part, '='); idx >= 0 {
			name = part[:idx]
		}
		if decoded, err := url.QueryUnescape(name); err == nil && decoded == "key" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "&")
}

// providerLocalError is a guard-generated failure rendered with the client's
// protocol envelope by the reverse proxy error handler.
type providerLocalError struct {
	status     int
	message    string
	retryAfter bool
}

func (e *providerLocalError) Error() string { return e.message }

var errProviderModeChanged = errors.New("provider mode changed")

func providerActionError(err error) error {
	if errors.Is(err, errProviderModeChanged) {
		return &providerLocalError{status: http.StatusNotFound, message: "Provider 已关闭，未执行响应动作"}
	}
	return &providerLocalError{status: http.StatusInternalServerError, message: "规则动作提交失败：" + err.Error()}
}

// providerTransport replays the spooled request for each attempt and judges
// 2xx responses when rules are active.
type providerTransport struct {
	base  http.RoundTripper
	state *providerProxyState
}

func (t *providerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st := t.state
	attempts := st.attempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := st.liveCheck(); err != nil {
			return nil, err
		}
		body, err := st.spool.Open()
		if err != nil {
			return nil, &providerLocalError{status: http.StatusInternalServerError, message: "读取请求体失败"}
		}
		out := req.Clone(req.Context())
		out.Body = body
		out.ContentLength = st.spool.Size()
		out.GetBody = nil
		out.Header = req.Header.Clone()
		resp, err := t.base.RoundTrip(out)
		if err != nil {
			_ = body.Close()
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || len(st.rules) == 0 {
			return resp, nil
		}
		actual, err := st.inspectResponse(resp)
		if err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
		if actual == "" {
			return resp, nil
		}
		rule := MatchResponseRule(st.rules, actual)
		if rule == nil {
			return resp, nil
		}
		if rule.Action == ResponseActionRecord {
			if err := st.record(actual, *rule); err != nil {
				_ = resp.Body.Close()
				return nil, providerActionError(err)
			}
			return resp, nil
		}
		// disable or reroute: this response must never reach the client.
		_ = resp.Body.Close()
		if attempt < attempts {
			continue
		}
		if err := st.record(actual, *rule); err != nil {
			return nil, providerActionError(err)
		}
		return providerLocalResponse(http.StatusServiceUnavailable, st.proto,
			fmt.Sprintf("模型守卫：返回模型 %s 未通过规则，已尝试 %d 次", actual, attempts), true), nil
	}
	return nil, &providerLocalError{status: http.StatusBadGateway, message: "上游模型请求失败"}
}

// liveCheck re-reads mode and block state before every attempt so a mid-flight
// console change stops further upstream calls.
func (st *providerProxyState) liveCheck() error {
	cfg, err := st.svc.store.LoadConfig()
	if err != nil {
		return &providerLocalError{status: http.StatusInternalServerError, message: "读取 Provider 配置失败"}
	}
	if st.supplier != nil {
		if err := st.supplier.live(); err != nil {
			return &providerLocalError{status: http.StatusServiceUnavailable, message: "供应商保护已关闭或配置冲突"}
		}
	} else if !cfg.ProviderEnabled {
		return &providerLocalError{status: http.StatusNotFound, message: "Provider 已关闭"}
	}
	if st.supplier != nil && st.supplier.probe {
		return nil
	}
	if st.target != nil {
		latest, err := st.svc.store.Target(st.target.Model)
		if err != nil {
			return &providerLocalError{status: http.StatusInternalServerError, message: "读取监测目标失败"}
		}
		if latest != nil && latest.blockedAt(time.Now()) {
			return &providerLocalError{status: http.StatusForbidden, message: "该模型已被守卫禁用"}
		}
	}
	return nil
}

// inspectResponse extracts the real model from a 2xx reply and replaces the
// body with a replayable equivalent. It returns an empty model when the reply
// is not JSON/SSE or carries no model, which means pass-through.
func (st *providerProxyState) inspectResponse(resp *http.Response) (string, error) {
	encoding := providerContentEncoding(resp.Header.Get("Content-Encoding"))
	if encoding == "other" {
		return "", &providerLocalError{status: http.StatusBadGateway, message: "上游响应编码不受支持"}
	}
	body := resp.Body
	decoded := false
	if encoding != "" {
		dec, err := providerDecodeReader(encoding, body)
		if err != nil {
			return "", &providerLocalError{status: http.StatusBadGateway, message: "解码上游响应失败"}
		}
		body, decoded = dec, true
	}
	mediaType := providerMediaType(resp.Header.Get("Content-Type"))
	switch {
	case mediaType == "text/event-stream":
		prefix, rest, actual, err := inspectProviderPrefix(resp.Request.Context(), body, providerStreamSSE)
		if err != nil {
			_ = body.Close()
			return "", &providerLocalError{status: http.StatusBadGateway, message: "读取上游流式响应失败"}
		}
		resp.Body = &providerPrependReader{prefix: prefix, rest: rest, closer: rest}
		if st.supplier != nil {
			st.svc.markSupplierReply(supplierStreamReplyID(prefix, providerStreamSSE))
		}
		if decoded {
			stripProviderEncoding(resp)
		}
		return actual, nil
	case mediaType == "application/x-ndjson" || mediaType == "application/ndjson" || mediaType == "application/jsonl":
		prefix, rest, actual, err := inspectProviderPrefix(resp.Request.Context(), body, providerStreamNDJSON)
		if err != nil {
			_ = body.Close()
			return "", &providerLocalError{status: http.StatusBadGateway, message: "读取上游流式响应失败"}
		}
		resp.Body = &providerPrependReader{prefix: prefix, rest: rest, closer: rest}
		if st.supplier != nil {
			st.svc.markSupplierReply(supplierStreamReplyID(prefix, providerStreamNDJSON))
		}
		if decoded {
			stripProviderEncoding(resp)
		}
		return actual, nil
	case providerMediaTypeIsJSON(mediaType):
		spool := newProviderSpool(st.svc.providerDataDir(), providerBodyLimit)
		if _, err := spool.ReadFrom(body); err != nil {
			_ = spool.Close()
			_ = body.Close()
			if errors.Is(err, errProviderTooLarge) {
				return "", &providerLocalError{status: http.StatusBadGateway, message: "上游响应超过检测大小上限"}
			}
			return "", &providerLocalError{status: http.StatusBadGateway, message: "读取上游响应失败"}
		}
		_ = body.Close()
		scan, err := spool.Open()
		if err != nil {
			_ = spool.Close()
			return "", &providerLocalError{status: http.StatusInternalServerError, message: "缓冲上游响应失败"}
		}
		values, scanErr := scanProviderJSON(scan, providerBodyLimit)
		_ = scan.Close()
		if scanErr != nil {
			_ = spool.Close()
			if errors.Is(scanErr, errProviderTooLarge) {
				return "", &providerLocalError{status: http.StatusBadGateway, message: "上游响应超过检测大小上限"}
			}
			return "", &providerLocalError{status: http.StatusBadGateway, message: "上游 JSON 响应无效"}
		}
		delivery, err := spool.Open()
		if err != nil {
			_ = spool.Close()
			return "", &providerLocalError{status: http.StatusInternalServerError, message: "缓冲上游响应失败"}
		}
		if st.supplier != nil {
			st.svc.markSupplierReply(values.ResponseID)
		}
		resp.Body = &providerSpoolReader{ReadCloser: delivery, spool: spool}
		// The replay serves exactly the spooled bytes; state the true length.
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Transfer-Encoding")
		resp.Header.Set("Content-Length", strconv.FormatInt(spool.Size(), 10))
		resp.ContentLength = spool.Size()
		return values.Model, nil
	default:
		resp.Body = body
		if decoded {
			stripProviderEncoding(resp)
		}
		return "", nil
	}
}

func providerMediaType(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if idx := strings.IndexByte(value, ';'); idx >= 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return value
}

func providerMediaTypeIsJSON(mediaType string) bool {
	if mediaType == "application/json" {
		return true
	}
	return strings.HasSuffix(mediaType, "+json")
}

func stripProviderEncoding(resp *http.Response) {
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
}

func providerLocalResponse(status int, proto providerProtocol, message string, retryAfter bool) *http.Response {
	body := providerErrorBody(proto, status, message)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if retryAfter {
		header.Set("Retry-After", "1")
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func providerErrorBody(proto providerProtocol, status int, message string) []byte {
	var value any
	switch proto {
	case providerProtoAnthropic:
		value = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    providerAnthropicErrorType(status),
				"message": message,
			},
		}
	case providerProtoGemini:
		value = map[string]any{
			"error": map[string]any{
				"code":    status,
				"status":  providerGoogleStatus(status),
				"message": message,
			},
		}
	case providerProtoOllama:
		value = map[string]any{"error": message}
	default:
		value = map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    "model_guard_error",
				"code":    status,
			},
		}
	}
	body, _ := json.Marshal(value)
	return body
}

func providerAnthropicErrorType(status int) string {
	switch status {
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadGateway:
		return "api_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusNotFound:
		return "not_found_error"
	}
	return "api_error"
}

func providerGoogleStatus(status int) string {
	switch status {
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusInternalServerError, http.StatusBadGateway:
		return "INTERNAL"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	}
	return "UNKNOWN"
}

func writeProviderError(w http.ResponseWriter, proto providerProtocol, status int, message string, retryAfter bool) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_, _ = w.Write(providerErrorBody(proto, status, message))
}

// providerIdleReader bounds each body read; a stalled upload ends with 408
// instead of hanging the handler forever.
type providerIdleReader struct {
	body    io.ReadCloser
	timeout time.Duration
	rc      *http.ResponseController
	fired   atomic.Bool
}

// Read bounds each body read. A connection-backed body gets the socket read
// deadline: closing such a body from a timer only queues behind the stalled
// read, so the timer path stays for bodies that support Close as an abort.
func (p *providerIdleReader) Read(b []byte) (int, error) {
	if p.rc != nil {
		if err := p.rc.SetReadDeadline(time.Now().Add(p.timeout)); err == nil {
			n, err := p.body.Read(b)
			if err != nil {
				var ne net.Error
				if errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
					return n, errProviderReadTimeout
				}
			}
			return n, err
		}
	}
	timer := time.AfterFunc(p.timeout, func() {
		p.fired.Store(true)
		_ = p.body.Close()
	})
	n, err := p.body.Read(b)
	timer.Stop()
	if err != nil && p.fired.Load() {
		return n, errProviderReadTimeout
	}
	return n, err
}

// spoolProviderRequest buffers a judged request body, validates the complete
// JSON document and extracts the requested model.
func (s *Service) spoolProviderRequest(w http.ResponseWriter, r *http.Request) (*providerSpool, providerScanValues, int, string) {
	var values providerScanValues
	spool := newProviderSpool(s.providerDataDir(), providerBodyLimit)
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	reader := &providerIdleReader{body: r.Body, timeout: providerIdleRead, rc: rc}
	if _, err := spool.ReadFrom(reader); err != nil {
		_ = spool.Close()
		var local *providerLocalError
		if errors.As(err, &local) {
			return nil, values, local.status, local.message
		}
		if errors.Is(err, errProviderTooLarge) {
			return nil, values, http.StatusRequestEntityTooLarge, "请求体超过 512 MiB 上限"
		}
		if errors.Is(err, errProviderReadTimeout) {
			return nil, values, http.StatusRequestTimeout, "读取请求体超时"
		}
		return nil, values, http.StatusBadRequest, "读取请求体失败"
	}
	encoding := providerContentEncoding(r.Header.Get("Content-Encoding"))
	if encoding == "other" {
		_ = spool.Close()
		return nil, values, http.StatusUnsupportedMediaType, errProviderUnsupportedEncoding.Error()
	}
	var body io.ReadCloser
	var err error
	body, err = spool.Open()
	if err != nil {
		_ = spool.Close()
		return nil, values, http.StatusInternalServerError, "读取请求体失败"
	}
	if encoding != "" {
		body, err = providerDecodeReader(encoding, body)
		if err != nil {
			_ = spool.Close()
			return nil, values, http.StatusBadRequest, "请求体编码无效"
		}
	}
	values, err = scanProviderJSONRequest(body, providerBodyLimit)
	_ = body.Close()
	if err != nil {
		_ = spool.Close()
		if errors.Is(err, errProviderTooLarge) {
			return nil, values, http.StatusRequestEntityTooLarge, "解码后的请求体超过 512 MiB 上限"
		}
		return nil, values, http.StatusBadRequest, "请求体不是有效 JSON 或编码损坏"
	}
	return spool, values, 0, ""
}

// providerDataDir keeps large replay spools on the service data volume.
func (s *Service) providerDataDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return os.TempDir()
}

// isProviderUpgrade reports a WebSocket handshake without depending on the
// connection package.
func isProviderUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// providerJSONModel extracts a model from a complete JSON document. It is kept
// for callers that already hold the bytes and for focused tests.
func providerJSONModel(body []byte) string {
	values, err := scanProviderJSON(bytes.NewReader(body), int64(len(body))+1)
	if err != nil {
		return ""
	}
	return values.Model
}
