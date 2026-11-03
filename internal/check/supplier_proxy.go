package check

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// supplierRequest binds an in-flight request to one durable takeover.
type supplierRequest struct {
	controller *supplierController
	binding    supplierBinding
	target     *url.URL
	transport  *http.Transport
	probe      bool
}

func (sr *supplierRequest) live() error {
	_, ok, err := sr.controller.binding(sr.binding.Token, sr.binding.Protocol)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("供应商保护已关闭或配置冲突")
	}
	return nil
}

func (s *Service) apiSuppliers(w http.ResponseWriter, r *http.Request) {
	if s.suppliers == nil {
		writeJSON(w, map[string]any{"configured": false, "providers": []any{}})
		return
	}
	list, err := s.suppliers.list(r.Context())
	if err != nil {
		errJSON(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, list)
}

func (s *Service) apiSupplierProtection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProviderID string `json:"provider_id"`
		Enabled    bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&input); err != nil {
		errJSON(w, 400, "请求体无效")
		return
	}
	if s.suppliers == nil {
		errJSON(w, 503, "未配置本机 Magpie 目录")
		return
	}
	if err := s.suppliers.setProtection(r.Context(), input.ProviderID, input.Enabled); err != nil {
		errJSON(w, 409, err.Error())
		return
	}
	s.apiSuppliers(w, r)
}

// apiSupplier proxies Magpie's already prepared vendor request, keeping its
// selected credential and exact body across response-rule retries.
func (s *Service) apiSupplier(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/upstream/"), "/", 3)
	if len(parts) != 3 || s.suppliers == nil {
		http.NotFound(w, r)
		return
	}
	binding, ok, err := s.suppliers.binding(parts[0], parts[1])
	proto := providerProtocol(parts[1])
	if err != nil {
		writeProviderError(w, proto, 503, "供应商保护配置不可用", false)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	sr := &supplierRequest{controller: s.suppliers, binding: binding}
	rest := "/" + parts[2]
	synthetic := *r.URL
	synthetic.Path, err = url.PathUnescape("/provider" + rest)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	synthetic.RawPath = "/provider" + rest
	parsed, valid := parseProviderPath(&synthetic)
	if !valid {
		http.NotFound(w, r)
		return
	}
	matched := matchProviderEndpoint(r.Method, parsed)
	// Vendor-specific auxiliary paths remain transparent. Only known conversation
	// endpoints enter judgment; the route token confines them to one vendor base.
	matched.endpoint.proto = proto
	cfg, err := s.store.LoadConfig()
	if err != nil {
		writeProviderError(w, proto, 500, "读取守卫配置失败", false)
		return
	}
	targetURL, err := supplierTarget(binding, rest, r.URL.RawQuery)
	if err != nil {
		writeProviderError(w, proto, 502, "供应商地址无效", false)
		return
	}
	transport, err := supplierTransport(binding, r, s.suppliers.configDir)
	if err != nil {
		writeProviderError(w, proto, 502, "供应商代理配置无效", false)
		return
	}
	if matched.endpoint.ws && isProviderUpgrade(r) {
		sr.target, sr.transport = targetURL, transport
		s.serveProviderSocket(w, r, matched, sr)
		return
	}
	var state *providerProxyState
	if matched.known && matched.allowed && matched.endpoint.gate && r.Method == http.MethodPost {
		spool, values, status, message := s.spoolProviderRequest(w, r)
		if status != 0 {
			writeProviderError(w, proto, status, message, false)
			return
		}
		defer spool.Close()
		model := values.Model
		if matched.endpoint.modelPath {
			model = matched.pathModel
		}
		requested, target, err := s.supplierModel(binding.ProviderID, model)
		if err != nil {
			writeProviderError(w, proto, 500, "读取模型配置失败", false)
			return
		}
		alias, err := s.store.Alias()
		if err != nil {
			writeProviderError(w, proto, 500, "读取探针配置失败", false)
			return
		}
		if r.Header.Get("X-Model-Guard-Session") == probeSession(ProbeAliasPrefix+alias+"/"+requested) {
			sr.probe = true
			target = nil
			cfg.ResponseModelMode = ResponseModelOff
		} else if target != nil && target.blockedAt(time.Now()) {
			writeProviderError(w, proto, 403, "该模型已被守卫禁用", false)
			return
		}
		rules := effectiveResponseRules(cfg, target)
		if !matched.endpoint.judge || values.Compaction || !hasActiveResponseRules(rules) {
			rules = nil
		}
		attempts := cfg.ProviderAttempts
		if attempts < 1 || attempts > 5 {
			attempts = 2
		}
		state = &providerProxyState{svc: s, proto: proto, spool: spool, requested: requested, target: target, rules: rules, attempts: attempts, supplier: sr}
	}
	var rt http.RoundTripper = transport
	if state != nil {
		rt = &providerTransport{base: transport, state: state}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = targetURL
			pr.Out.Host = targetURL.Host
			stripSupplierHeaders(pr.Out.Header)
			if state != nil {
				pr.Out.Body = nil
				pr.Out.GetBody = nil
				pr.Out.ContentLength = state.spool.Size()
				if len(state.rules) > 0 {
					pr.Out.Header.Set("Accept-Encoding", "identity")
				}
			}
		},
		Transport: rt, FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() != nil {
				return
			}
			var local *providerLocalError
			if errors.As(err, &local) {
				writeProviderError(rw, proto, local.status, local.message, local.retryAfter)
				return
			}
			// Transport errors can contain vendor URLs with query credentials.
			writeProviderError(rw, proto, 502, "上游供应商请求失败", false)
		},
	}
	proxy.ServeHTTP(w, r)
}

func stripSupplierHeaders(h http.Header) {
	for _, key := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Proxy-Authorization", "Proxy-Authenticate", "X-Model-Guard-Session"} {
		h.Del(key)
	}
}

func supplierTarget(binding supplierBinding, rest, query string) (*url.URL, error) {
	u, err := supplierNormalizeVendorBase(binding.Protocol, binding.Base)
	if err != nil {
		return nil, err
	}
	switch binding.Protocol {
	case "chat", "responses", "decide":
		rest = strings.TrimPrefix(rest, "/v1")
	case "gemini":
		rest = strings.TrimPrefix(rest, "/v1beta")
	}
	escaped := strings.TrimRight(u.EscapedPath(), "/") + rest
	u.Path, err = url.PathUnescape(escaped)
	if err != nil {
		return nil, err
	}
	u.RawPath = escaped
	u.RawQuery = query
	return u, nil
}

type supplierSettings struct {
	Proxy      string            `json:"proxy"`
	ModelIDs   map[string]string `json:"modelIDs"`
	ModelWires map[string]string `json:"modelWires"`
}

func readSupplierSettings(dir string) (supplierSettings, error) {
	var out supplierSettings
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

// supplierModel reverses only unambiguous wire names among monitored targets.
func (s *Service) supplierModel(provider, wire string) (string, *Target, error) {
	settings, err := readSupplierSettings(s.suppliers.configDir)
	if err != nil {
		return "", nil, err
	}
	targets, err := s.store.Targets()
	if err != nil {
		return "", nil, err
	}
	var match *Target
	for _, t := range targets {
		local, ok := strings.CutPrefix(t.Model, provider+"/")
		if !ok {
			continue
		}
		own := strings.TrimSpace(settings.ModelIDs[t.Model])
		name := strings.TrimSpace(settings.ModelWires[t.Model])
		if name != "" {
			name = strings.ReplaceAll(name, "*", local)
		} else {
			if own == "" {
				own = local
			}
			if pattern := strings.TrimSpace(settings.ModelWires[provider+"/*"]); pattern != "" {
				name = strings.ReplaceAll(pattern, "*", own)
			} else {
				name = own
			}
		}
		if name == wire {
			if match != nil {
				return provider + "/" + wire, nil, nil
			}
			match = t
		}
	}
	if match != nil {
		return match.Model, match, nil
	}
	return provider + "/" + wire, nil, nil
}

var supplierTransports sync.Map

func supplierTransport(binding supplierBinding, r *http.Request, dir string) (*http.Transport, error) {
	choice := binding.Proxy
	key := supplierCredentialKey(r)
	if key != "" {
		sum := sha256.Sum256([]byte(key))
		if own := binding.AccountProxies[hex.EncodeToString(sum[:5])]; own != "" {
			choice = own
		}
	}
	ownProxy := choice != ""
	if choice == "" {
		settings, err := readSupplierSettings(dir)
		if err != nil {
			return nil, err
		}
		choice = settings.Proxy
	}
	cacheKey := struct {
		Proxy string
		TLS   bool
		Own   bool
	}{choice, binding.InsecureTLS, ownProxy}
	if cached, ok := supplierTransports.Load(cacheKey); ok {
		return cached.(*http.Transport), nil
	}
	t := providerUpstreamTransport.Clone()
	t.Proxy = supplierDefaultProxy
	if choice == "direct" {
		t.Proxy = nil
	} else if choice != "" {
		if !strings.Contains(choice, "://") {
			choice = "http://" + choice
		}
		u, err := url.Parse(choice)
		if err != nil || u.Host == "" {
			return nil, errors.New("invalid proxy")
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, errors.New("unsupported proxy")
		}
		t.Proxy = func(req *http.Request) (*url.URL, error) {
			host := req.URL.Hostname()
			if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) || (!ownProxy && supplierLocalHost(host)) {
				return nil, nil
			}
			return u, nil
		}
	}
	if binding.InsecureTLS {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	} // Only this explicitly opted-in vendor transport uses it.
	t.ResponseHeaderTimeout = 0
	actual, loaded := supplierTransports.LoadOrStore(cacheKey, t)
	if loaded {
		t.CloseIdleConnections()
	}
	return actual.(*http.Transport), nil
}

func (s *Service) recordSupplierResponse(sr *supplierRequest, requested, actual string, target *Target, rule ResponseModelRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := sr.live(); err != nil {
		return errProviderModeChanged
	}
	cfg, err := s.store.LoadConfig()
	if err != nil {
		return err
	}
	// Route isolation removes the selected Magpie member, whose id can differ
	// from the vendor's returned model (aliases and modelWires).
	needsSync, err := s.applyResponseRuleForRouteLocked(cfg, sr.binding.ProviderID, requested, actual, requested, target, rule, time.Now().UTC())
	if err != nil {
		return err
	}
	if needsSync {
		if err := s.publishGate(); err != nil {
			delete(s.responseHits, responseHitKey(requested, actual, rule))
			return err
		}
	}
	return nil
}

func (st *providerProxyState) record(actual string, rule ResponseModelRule) error {
	return st.svc.recordSupplierResponse(st.supplier, st.requested, actual, st.target, rule)
}

// logActionError records a failed rule submission so the console explains a
// 500 without hiding the cause in Health.
func (st *providerProxyState) logActionError(actual string, rule ResponseModelRule, err error) {
	st.svc.logSupplierEvent(&SupplierEvent{
		Kind:        "error",
		ProviderID:  st.supplier.binding.ProviderID,
		Model:       st.requested,
		ActualModel: actual,
		Action:      string(rule.Action),
		RuleID:      rule.ID,
		Detail:      "规则动作提交失败：" + err.Error(),
	})
}

// supplierCredentialKey reads the credential Magpie already prepared for the
// selected vendor key, including Azure's api-key header, so a per-key proxy
// follows the key that actually signs this request.
func supplierCredentialKey(r *http.Request) string {
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if key == "" {
		key = r.Header.Get("X-Api-Key")
	}
	if key == "" {
		key = r.Header.Get("Api-Key")
	}
	if key == "" {
		key = r.Header.Get("X-Goog-Api-Key")
	}
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	return key
}

func supplierSocketHeaders(r *http.Request) http.Header {
	h := r.Header.Clone()
	for _, name := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(name))
	}
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Protocol", "Sec-Websocket-Extensions", "Host", "Origin"} {
		h.Del(name)
	}
	stripSupplierHeaders(h)
	return h
}

// probeSession fits Magpie's 128-byte session limit even for long model ids.
func probeSession(aliasedModel string) string {
	sum := sha256.Sum256([]byte(aliasedModel))
	return ProbeAliasPrefix + hex.EncodeToString(sum[:])
}
