package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	providerSocketLimit      int64 = 512 << 20
	providerSocketQueueLimit int64 = 512 << 20
	providerSocketCacheLimit       = 64 << 10
	providerSocketFrameKeep        = 8 << 20
)

// The waits are variables so tests can shorten them.
var (
	providerSocketModelWait      = 2 * time.Second
	providerSocketInterruptWait  = 2 * time.Second
	providerSocketInterruptQuiet = 100 * time.Millisecond
)

type providerClientEvent struct {
	spool *providerSpool
	scan  providerScanValues
	err   error
}

type providerBackendKind int

const (
	providerBackendStart providerBackendKind = iota + 1
	providerBackendChunk
	providerBackendEnd
	providerBackendError
)

type providerBackendEvent struct {
	kind providerBackendKind
	typ  websocket.MessageType
	data []byte
	err  error
}

type providerSocket struct {
	svc     *Service
	backend *websocket.Conn
	client  *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc

	clientCh  chan providerClientEvent
	backendCh chan providerBackendEvent
	interrupt chan string

	clientWriteMu  sync.Mutex
	backendWriteMu sync.Mutex

	pendingBytes atomic.Int64
	supplier     *supplierRequest
}

// serveProviderSocket connects one front-end Responses socket to one taken-over
// supplier socket and judges each turn on that connection.
func (s *Service) serveProviderSocket(w http.ResponseWriter, r *http.Request, matched providerMatch, sr *supplierRequest) {
	// Same-origin only, using the same rule as websocket.Accept: the Origin
	// host must be the request host. Native clients without Origin pass.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(r.Host, u.Host) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
	}
	target := *sr.target
	targetURL := &target
	switch targetURL.Scheme {
	case "https":
		targetURL.Scheme = "wss"
	default:
		targetURL.Scheme = "ws"
	}
	headers := supplierSocketHeaders(r)
	httpClient := &http.Client{Transport: sr.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	dialCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	backend, resp, err := websocket.Dial(dialCtx, targetURL.String(), &websocket.DialOptions{
		HTTPHeader:      headers,
		HTTPClient:      httpClient,
		Subprotocols:    providerClientSubprotocols(r),
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	cancel()
	if err != nil {
		if resp != nil {
			copyProviderFailureResponse(w, resp)
			return
		}
		writeProviderError(w, matched.endpoint.proto, http.StatusBadGateway, "连接原生 Responses WebSocket 失败："+err.Error(), false)
		return
	}
	// Only now accept the front-end, so a native refusal is reported as HTTP.
	var subprotocols []string
	if sp := backend.Subprotocol(); sp != "" {
		subprotocols = []string{sp}
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    subprotocols,
		CompressionMode: websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		_ = backend.CloseNow()
		return
	}
	client.SetReadLimit(providerSocketLimit)
	backend.SetReadLimit(providerSocketLimit)
	ctx, cancelCtx := context.WithCancel(r.Context())
	ps := &providerSocket{
		svc:       s,
		backend:   backend,
		client:    client,
		ctx:       ctx,
		cancel:    cancelCtx,
		clientCh:  make(chan providerClientEvent, 64),
		backendCh: make(chan providerBackendEvent),
		interrupt: make(chan string, 1),
		supplier:  sr,
	}
	defer cancelCtx()
	defer ps.client.CloseNow()
	defer ps.backend.CloseNow()
	ps.serve()
}

func providerClientSubprotocols(r *http.Request) []string {
	raw := r.Header.Get("Sec-WebSocket-Protocol")
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func copyProviderFailureResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for key, values := range resp.Header {
		switch http.CanonicalHeaderKey(key) {
		case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
			"Content-Length", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (ps *providerSocket) serve() {
	defer func() {
		ps.cancel()
		ps.drainClientQueue()
	}()
	go ps.readClient()
	go ps.readBackend()
	for {
		select {
		case ev := <-ps.clientCh:
			if ev.err != nil {
				ps.cancel()
				return
			}
			if ev.spool == nil {
				continue
			}
			ps.runTurn(ev)
			if ps.ctx.Err() != nil {
				return
			}
		case ev := <-ps.backendCh:
			ps.forwardDirect(ev)
			if ps.ctx.Err() != nil {
				return
			}
		case <-ps.ctx.Done():
			return
		}
	}
}

// readClient parses each client frame. Creates are queued for serial handling;
// interrupts and other frames are forwarded immediately, as native does.
func (ps *providerSocket) readClient() {
	for {
		typ, reader, err := ps.client.Reader(ps.ctx)
		if err != nil {
			ps.cancel()
			return
		}
		if typ != websocket.MessageText {
			_ = ps.client.Close(websocket.StatusUnsupportedData, "text frames only")
			ps.cancel()
			return
		}
		spool := newProviderSpool(ps.svc.providerDataDir(), providerSocketLimit)
		if _, err := spool.ReadFrom(reader); err != nil {
			_ = spool.Close()
			if errors.Is(err, errProviderTooLarge) {
				_ = ps.client.Close(websocket.StatusMessageTooBig, "frame too large")
			}
			ps.cancel()
			return
		}
		scan := providerScanSpool(spool)
		switch scan.Type {
		case "response.interrupt":
			if err := ps.sendSpool(spool); err != nil {
				_ = spool.Close()
				ps.cancel()
				return
			}
			_ = spool.Close()
			select {
			case ps.interrupt <- scan.ResponseID:
			default:
			}
		case "response.create":
			ps.pendingBytes.Add(spool.Size())
			select {
			case ps.clientCh <- providerClientEvent{spool: spool, scan: scan}:
			case <-ps.ctx.Done():
				_ = spool.Close()
				return
			default:
				_ = spool.Close()
				_ = ps.client.Close(websocket.StatusMessageTooBig, "too many queued turns")
				ps.cancel()
				return
			}
			if ps.pendingBytes.Load() > providerSocketQueueLimit {
				_ = ps.client.Close(websocket.StatusMessageTooBig, "queued turns too large")
				ps.cancel()
				return
			}
		default:
			if err := ps.sendSpool(spool); err != nil {
				_ = spool.Close()
				ps.cancel()
				return
			}
			_ = spool.Close()
		}
	}
}

// drainClientQueue closes creates still queued when the socket ends. Large
// frames spool to disk, so leaving them behind would retain prompt data.
func (ps *providerSocket) drainClientQueue() {
	for {
		select {
		case ev := <-ps.clientCh:
			if ev.spool != nil {
				_ = ev.spool.Close()
				ps.pendingBytes.Add(-ev.spool.Size())
			}
		default:
			return
		}
	}
}

func providerScanSpool(spool *providerSpool) providerScanValues {
	reader, err := spool.Open()
	if err != nil {
		return providerScanValues{}
	}
	defer reader.Close()
	values, err := scanProviderJSONRequest(reader, providerSocketLimit)
	if err != nil {
		return providerScanValues{}
	}
	return values
}

// readBackend turns one native message at a time into start/chunk/end events.
func (ps *providerSocket) readBackend() {
	for {
		typ, reader, err := ps.backend.Reader(ps.ctx)
		if err != nil {
			select {
			case ps.backendCh <- providerBackendEvent{kind: providerBackendError, err: err}:
			case <-ps.ctx.Done():
			}
			return
		}
		select {
		case ps.backendCh <- providerBackendEvent{kind: providerBackendStart, typ: typ}:
		case <-ps.ctx.Done():
			return
		}
		buf := make([]byte, providerReadChunkSize)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				data := append([]byte(nil), buf[:n]...)
				select {
				case ps.backendCh <- providerBackendEvent{kind: providerBackendChunk, data: data}:
				case <-ps.ctx.Done():
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					select {
					case ps.backendCh <- providerBackendEvent{kind: providerBackendEnd}:
					case <-ps.ctx.Done():
						return
					}
					// The next message follows on the same connection.
					break
				}
				select {
				case ps.backendCh <- providerBackendEvent{kind: providerBackendError, err: err}:
				case <-ps.ctx.Done():
				}
				return
			}
		}
	}
}

func (ps *providerSocket) writeClientMessage(ctx context.Context, data []byte) error {
	ps.clientWriteMu.Lock()
	defer ps.clientWriteMu.Unlock()
	return ps.client.Write(ctx, websocket.MessageText, data)
}

func (ps *providerSocket) clientWriter(ctx context.Context) (io.WriteCloser, error) {
	ps.clientWriteMu.Lock()
	w, err := ps.client.Writer(ctx, websocket.MessageText)
	if err != nil {
		ps.clientWriteMu.Unlock()
		return nil, err
	}
	return &providerSocketWriter{w: w, mu: &ps.clientWriteMu}, nil
}

type providerSocketWriter struct {
	w  io.WriteCloser
	mu *sync.Mutex
}

func (w *providerSocketWriter) Write(p []byte) (int, error) { return w.w.Write(p) }

func (w *providerSocketWriter) Close() error {
	err := w.w.Close()
	w.mu.Unlock()
	return err
}

func (ps *providerSocket) sendSpool(spool *providerSpool) error {
	reader, err := spool.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	ps.backendWriteMu.Lock()
	defer ps.backendWriteMu.Unlock()
	w, err := ps.backend.Writer(ps.ctx, websocket.MessageText)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, reader); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// forwardDirect relays one backend frame when no turn is judging it.
func (ps *providerSocket) forwardDirect(ev providerBackendEvent) {
	switch ev.kind {
	case providerBackendStart:
		w, err := ps.clientWriter(ps.ctx)
		if err != nil {
			ps.cancel()
			return
		}
		for {
			select {
			case next := <-ps.backendCh:
				switch next.kind {
				case providerBackendChunk:
					if _, err := w.Write(next.data); err != nil {
						_ = w.Close()
						ps.cancel()
						return
					}
				case providerBackendEnd:
					_ = w.Close()
					return
				case providerBackendError:
					_ = w.Close()
					ps.mirrorClose(next.err)
					return
				case providerBackendStart:
					_ = w.Close()
					ps.forwardDirect(next)
					return
				}
			case <-ps.ctx.Done():
				_ = w.Close()
				return
			}
		}
	case providerBackendError:
		ps.mirrorClose(ev.err)
	}
}

func (ps *providerSocket) mirrorClose(err error) {
	code := websocket.CloseStatus(err)
	if code == -1 {
		code = websocket.StatusInternalError
	}
	_ = ps.client.Close(code, "")
	ps.cancel()
}

func (ps *providerSocket) runTurn(ev providerClientEvent) {
	defer ev.spool.Close()
	ps.pendingBytes.Add(-ev.spool.Size())
	ps.drainInterrupts()

	cfg, err := ps.svc.store.LoadConfig()
	if err != nil || ps.supplier.live() != nil {
		_ = ps.client.Close(websocket.StatusPolicyViolation, "supplier protection disabled")
		ps.cancel()
		return
	}
	model := strings.TrimSpace(ev.scan.Model)
	model, target, err := ps.svc.supplierModel(ps.supplier.binding.ProviderID, model)
	if err != nil {
		ps.failTurn(500, "model_guard_action_failed", "读取模型配置失败")
		return
	}
	if target != nil && target.blockedAt(time.Now()) {
		ps.sendSocketError(http.StatusForbidden, "model_guard_blocked", "该模型已被守卫禁用")
		_ = ps.client.Close(websocket.StatusPolicyViolation, "model disabled")
		ps.cancel()
		return
	}
	rules := effectiveResponseRules(cfg, target)
	if !hasActiveResponseRules(rules) {
		rules = nil
	}
	if ev.scan.HasGenerate && !ev.scan.Generate {
		// Warmup: relay as-is, never judge its requested model as the reply.
		rules = nil
	}
	attempts := cfg.ProviderAttempts
	if attempts < 1 || attempts > 5 {
		attempts = 2
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if ps.supplier.live() != nil {
				ps.failTurn(http.StatusServiceUnavailable, "model_guard_disabled", "供应商保护已关闭")
				return
			}
			latest, err := ps.svc.store.Target(model)
			if err != nil {
				ps.failTurn(http.StatusInternalServerError, "model_guard_action_failed", "读取监测目标失败")
				return
			}
			if latest != nil && latest.blockedAt(time.Now()) {
				ps.sendSocketError(http.StatusForbidden, "model_guard_blocked", "该模型已被守卫禁用")
				_ = ps.client.Close(websocket.StatusPolicyViolation, "model disabled")
				ps.cancel()
				return
			}
		}
		if err := ps.sendSpool(ev.spool); err != nil {
			ps.cancel()
			return
		}
		if len(rules) == 0 {
			ps.passthroughTurn()
			return
		}
		if ps.judgeAttempt(model, target, rules, attempts, attempt) != providerTurnRetry {
			return
		}
	}
}

func (ps *providerSocket) drainInterrupts() {
	for {
		select {
		case <-ps.interrupt:
		default:
			return
		}
	}
}

type providerTurnOutcome int

const (
	providerTurnDone providerTurnOutcome = iota
	providerTurnRetry
)

// judgeAttempt caches the turn's frames until a model appears or a boundary is
// reached. It returns retry only after a clean internal interrupt.
func (ps *providerSocket) judgeAttempt(requested string, target *Target, rules []ResponseModelRule, attempts, attempt int) providerTurnOutcome {
	timer := time.NewTimer(providerSocketModelWait)
	defer timer.Stop()
	var cached [][]byte
	var cachedBytes int
	var responseID string
	var sawCreated bool
	var current []byte
	inFrame := false

	flush := func(extra []byte) {
		for _, frame := range cached {
			if err := ps.writeClientMessage(ps.ctx, frame); err != nil {
				return
			}
		}
		if extra != nil {
			_ = ps.writeClientMessage(ps.ctx, extra)
		}
	}
	pass := func(extra []byte) providerTurnOutcome {
		flush(extra)
		ps.passthroughTurn()
		return providerTurnDone
	}
	// A judged turn that already holds its terminal frame is finished; waiting
	// for another terminal event would keep the next create queued forever.
	flushOnly := func(extra []byte) providerTurnOutcome {
		flush(extra)
		return providerTurnDone
	}

	for {
		select {
		case <-ps.ctx.Done():
			return providerTurnDone
		case <-ps.interrupt:
			// The client interrupted the turn itself; stop judging, never retry.
			return pass(nil)
		case <-timer.C:
			if inFrame {
				for _, frame := range cached {
					if err := ps.writeClientMessage(ps.ctx, frame); err != nil {
						return providerTurnDone
					}
				}
				cached = nil
				terminal, err := ps.streamFrame(current)
				if err != nil {
					ps.mirrorClose(err)
					return providerTurnDone
				}
				if terminal {
					return providerTurnDone
				}
			} else {
				flush(nil)
			}
			ps.passthroughTurn()
			return providerTurnDone
		case ev := <-ps.backendCh:
			switch ev.kind {
			case providerBackendStart:
				inFrame, current = true, current[:0]
			case providerBackendChunk:
				if !inFrame {
					continue
				}
				current = append(current, ev.data...)
				if cachedBytes+len(current) > providerSocketCacheLimit {
					for _, frame := range cached {
						if err := ps.writeClientMessage(ps.ctx, frame); err != nil {
							return providerTurnDone
						}
					}
					cached = nil
					terminal, err := ps.streamFrame(current)
					if err != nil {
						ps.mirrorClose(err)
						return providerTurnDone
					}
					if terminal {
						return providerTurnDone
					}
					ps.passthroughTurn()
					return providerTurnDone
				}
			case providerBackendEnd:
				if !inFrame {
					continue
				}
				inFrame = false
				frame := append([]byte(nil), current...)
				values := providerFrameValues(frame)
				if ps.supplier != nil {
					ps.svc.markSupplierReply(values.ResponseID)
				}
				if values.ResponseID != "" {
					responseID = values.ResponseID
				}
				if values.Type == "response.created" || values.Type == "response.in_progress" {
					sawCreated = true
				}
				terminal := providerTerminalType(values.Type)
				rule := (*ResponseModelRule)(nil)
				if values.Model != "" {
					rule = MatchResponseRule(rules, values.Model)
				}
				switch {
				case rule == nil:
					// The vendor's model can appear after the first frame (a
					// translated chat stream names it late). Keep judging rather
					// than passing the turn unread; a terminal frame with no
					// matching rule flushes what was cached.
					cached = append(cached, frame)
					cachedBytes += len(frame)
					if !terminal {
						continue
					}
					return flushOnly(nil)
				case rule.Action == ResponseActionRecord:
					if err := ps.svc.recordSupplierResponse(ps.supplier, requested, values.Model, target, *rule); err != nil {
						ps.logActionError(requested, values.Model, *rule, err)
						ps.actionFailed(err)
						return providerTurnDone
					}
					cached = append(cached, frame)
					if terminal {
						return flushOnly(nil)
					}
					return pass(nil)
				case terminal:
					// The bad reply already completed; never deliver it.
					ps.blockTurn(requested, values.Model, target, *rule, attempts)
					return providerTurnDone
				case attempt < attempts && responseID != "" && sawCreated:
					if ps.interruptAndWait(responseID) {
						ps.drainBackendQuiet()
						providerID := ""
						if ps.supplier != nil {
							providerID = ps.supplier.binding.ProviderID
						}
						ps.svc.logSupplierEvent(&SupplierEvent{
							Kind:        "retry",
							ProviderID:  providerID,
							Model:       requested,
							ActualModel: values.Model,
							Action:      string(rule.Action),
							RuleID:      rule.ID,
							Detail:      fmt.Sprintf("第 %d/%d 次尝试返回模型 %s，命中规则 %s，已重试", attempt, attempts, values.Model, rule.Pattern),
						})
						return providerTurnRetry
					}
					ps.blockTurn(requested, values.Model, target, *rule, attempts)
					return providerTurnDone
				default:
					if responseID != "" {
						ps.interruptAndWait(responseID)
					}
					ps.blockTurn(requested, values.Model, target, *rule, attempts)
					return providerTurnDone
				}
			case providerBackendError:
				ps.mirrorClose(ev.err)
				return providerTurnDone
			}
		}
	}
}

func providerFrameValues(frame []byte) providerScanValues {
	values, err := scanProviderJSON(bytes.NewReader(frame), int64(len(frame))+1)
	if err != nil {
		return providerScanValues{}
	}
	return values
}

// providerFrameTypeScanner finds the top-level "type" of a JSON object as it
// arrives, without buffering the whole frame. Nested "type" values (such as an
// output item's content type) are ignored. It survives chunk boundaries.
type providerFrameTypeScanner struct {
	depth        int
	inString     bool
	escaped      bool
	tokenDepth   int
	token        []byte
	tokenOver    bool
	afterString  bool
	lastString   string
	wantValue    bool
	inValue      bool
	valueEscaped bool
	value        []byte
	valueOver    bool
	typ          string
}

func (s *providerFrameTypeScanner) Write(chunk []byte) {
	for _, c := range chunk {
		if s.typ != "" {
			return
		}
		if s.inValue {
			if s.valueEscaped {
				s.valueEscaped = false
				if len(s.value) < 64 {
					s.value = append(s.value, c)
				} else {
					s.valueOver = true
				}
				continue
			}
			switch c {
			case '\\':
				s.valueEscaped = true
			case '"':
				if !s.valueOver {
					s.typ = string(s.value)
				}
				s.inValue = false
				continue
			default:
				if len(s.value) < 64 {
					s.value = append(s.value, c)
				} else {
					s.valueOver = true
				}
			}
			continue
		}
		if s.inString {
			if s.escaped {
				s.escaped = false
				if s.tokenDepth == 1 && len(s.token) < 64 {
					s.token = append(s.token, c)
				}
				continue
			}
			if c == '\\' {
				s.escaped = true
				continue
			}
			if c == '"' {
				s.inString = false
				if s.tokenDepth == 1 {
					s.afterString = true
					if s.tokenOver {
						s.lastString = ""
					} else {
						s.lastString = string(s.token)
					}
				}
				continue
			}
			if s.tokenDepth == 1 {
				if len(s.token) < 64 {
					s.token = append(s.token, c)
				} else {
					s.tokenOver = true
				}
			}
			continue
		}
		switch c {
		case '"':
			if s.wantValue && s.depth == 1 {
				s.inValue = true
				s.value = s.value[:0]
				s.valueOver = false
				s.valueEscaped = false
				continue
			}
			s.inString = true
			s.tokenDepth = s.depth
			s.token = s.token[:0]
			s.tokenOver = false
		case ':':
			if s.afterString && s.depth == 1 && s.lastString == "type" {
				s.wantValue = true
			}
			s.afterString = false
		case ',':
			s.afterString = false
			s.wantValue = false
		case '{', '[':
			s.depth++
			s.afterString = false
		case '}', ']':
			if s.depth > 0 {
				s.depth--
			}
			s.afterString = false
			s.wantValue = false
		}
	}
}

func (s *providerFrameTypeScanner) Type() string { return s.typ }

// providerFrameType reads the top-level type from one complete frame.
func providerFrameType(frame []byte) string {
	var scanner providerFrameTypeScanner
	scanner.Write(frame)
	return scanner.Type()
}

func providerTerminalType(kind string) bool {
	switch kind {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	}
	return false
}

// streamFrame forwards the rest of one in-progress frame and reports whether it
// was terminal. The prefix is delivered before the frame ends.
func (ps *providerSocket) streamFrame(prefix []byte) (bool, error) {
	w, err := ps.clientWriter(ps.ctx)
	if err != nil {
		return false, err
	}
	var scanner providerFrameTypeScanner
	scanner.Write(prefix)
	if _, err := w.Write(prefix); err != nil {
		_ = w.Close()
		return false, err
	}
	for {
		select {
		case ev := <-ps.backendCh:
			switch ev.kind {
			case providerBackendChunk:
				if _, err := w.Write(ev.data); err != nil {
					_ = w.Close()
					return false, err
				}
				scanner.Write(ev.data)
			case providerBackendEnd:
				_ = w.Close()
				return providerTerminalType(scanner.Type()), nil
			case providerBackendError:
				_ = w.Close()
				return false, ev.err
			case providerBackendStart:
				_ = w.Close()
				return false, nil
			}
		case <-ps.ctx.Done():
			_ = w.Close()
			return false, ps.ctx.Err()
		}
	}
}

// passthroughTurn forwards frames until the turn's terminal frame so the next
// create is never sent while the previous reply is still streaming.
func (ps *providerSocket) passthroughTurn() {
	for {
		select {
		case ev := <-ps.backendCh:
			switch ev.kind {
			case providerBackendStart:
				terminal, err := ps.streamFrame(nil)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						ps.mirrorClose(err)
					}
					return
				}
				if terminal {
					return
				}
			case providerBackendError:
				ps.mirrorClose(ev.err)
				return
			}
		case <-ps.ctx.Done():
			return
		}
	}
}

func (ps *providerSocket) sendInternalInterrupt(id string) error {
	frame, _ := json.Marshal(map[string]any{"type": "response.interrupt", "response_id": id})
	ps.backendWriteMu.Lock()
	defer ps.backendWriteMu.Unlock()
	return ps.backend.Write(ps.ctx, websocket.MessageText, frame)
}

// interruptAndWait sends an internal interrupt and consumes the turn's frames
// until both accepted and incomplete are seen. Any other outcome means the
// turn cannot be safely replayed on this socket.
func (ps *providerSocket) interruptAndWait(id string) bool {
	if id == "" {
		return false
	}
	if err := ps.sendInternalInterrupt(id); err != nil {
		return false
	}
	timer := time.NewTimer(providerSocketInterruptWait)
	defer timer.Stop()
	accepted, incomplete := false, false
	var buf bytes.Buffer
	var scanner providerFrameTypeScanner
	assembling := false
	for {
		select {
		case <-ps.ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ps.interrupt:
			// The client interrupted concurrently; attribution is ambiguous.
			return false
		case ev := <-ps.backendCh:
			switch ev.kind {
			case providerBackendStart:
				assembling = true
				buf.Reset()
				scanner = providerFrameTypeScanner{}
			case providerBackendChunk:
				if assembling {
					scanner.Write(ev.data)
					if buf.Len() < providerSocketFrameKeep {
						buf.Write(ev.data)
					}
				}
			case providerBackendEnd:
				if !assembling {
					continue
				}
				assembling = false
				values := providerFrameValues(buf.Bytes())
				switch scanner.Type() {
				case "response.interrupt.accepted":
					if values.ResponseID == "" || values.ResponseID == id {
						accepted = true
					}
				case "response.interrupt.failed":
					return false
				case "response.incomplete":
					if values.ResponseID == "" || values.ResponseID == id {
						incomplete = true
					}
				case "response.completed", "response.failed", "error":
					return false
				}
				if accepted && incomplete {
					return true
				}
			case providerBackendError:
				return false
			}
		}
	}
}

// drainBackendQuiet discards frames left over after an interrupted turn. The
// vendor may finish the aborted reply after response.incomplete; judging those
// stale frames as the retry would block a turn that was never replayed.
func (ps *providerSocket) drainBackendQuiet() {
	timer := time.NewTimer(providerSocketInterruptQuiet)
	defer timer.Stop()
	for {
		select {
		case <-ps.ctx.Done():
			return
		case <-timer.C:
			return
		case ev := <-ps.backendCh:
			if ev.kind == providerBackendError {
				return
			}
			timer.Reset(providerSocketInterruptQuiet)
		}
	}
}

func (ps *providerSocket) failTurn(status int, code, message string) {
	ps.sendSocketError(status, code, message)
	_ = ps.client.Close(websocket.StatusInternalError, "model guard failed")
	ps.cancel()
}

func (ps *providerSocket) blockTurn(requested, actual string, target *Target, rule ResponseModelRule, attempts int) {
	if err := ps.svc.recordSupplierResponse(ps.supplier, requested, actual, target, rule); err != nil {
		ps.logActionError(requested, actual, rule, err)
		ps.actionFailed(err)
		return
	}
	message := fmt.Sprintf("模型守卫：返回模型 %s 未通过规则，已尝试 %d 次；请重连并重发完整输入", actual, attempts)
	ps.sendSocketError(http.StatusServiceUnavailable, "model_guard_blocked", message)
	_ = ps.client.Close(websocket.StatusInternalError, "model guard blocked")
	ps.cancel()
}

func (ps *providerSocket) logActionError(requested, actual string, rule ResponseModelRule, err error) {
	providerID := ""
	if ps.supplier != nil {
		providerID = ps.supplier.binding.ProviderID
	}
	ps.svc.logSupplierEvent(&SupplierEvent{
		Kind:        "error",
		ProviderID:  providerID,
		Model:       requested,
		ActualModel: actual,
		Action:      string(rule.Action),
		RuleID:      rule.ID,
		Detail:      "规则动作提交失败：" + err.Error(),
	})
}

func (ps *providerSocket) actionFailed(err error) {
	if errors.Is(err, errProviderModeChanged) {
		_ = ps.client.Close(websocket.StatusPolicyViolation, "provider disabled")
		ps.cancel()
		return
	}
	ps.sendSocketError(http.StatusInternalServerError, "model_guard_action_failed", "规则动作提交失败："+err.Error())
	_ = ps.client.Close(websocket.StatusInternalError, "model guard action failed")
	ps.cancel()
}

func (ps *providerSocket) sendSocketError(status int, code, message string) {
	frame, _ := json.Marshal(map[string]any{
		"type":   "error",
		"status": status,
		"error": map[string]any{
			"type":    "model_guard_error",
			"code":    code,
			"message": message,
		},
	})
	_ = ps.writeClientMessage(ps.ctx, frame)
}
