package check

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newSocketEnv(t *testing.T, handler func(ctx context.Context, c *websocket.Conn)) *supplierEnv {
	t.Helper()
	return newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if !isProviderUpgrade(r) {
			http.Error(w, "upgrade required", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(providerSocketLimit)
		handler(r.Context(), c)
		// A close immediately after the last write can truncate buffered
		// frames; drain until the guard closes this backend socket.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})
}

func dialGuardSocket(t *testing.T, env *supplierEnv, target, origin string) *websocket.Conn {
	t.Helper()
	target = "ws" + strings.TrimPrefix(target, "http")
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial guard: %v (status %d)", err, status)
	}
	c.SetReadLimit(providerSocketLimit)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func writeSocketJSON(t *testing.T, c *websocket.Conn, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(value)); err != nil {
		t.Fatalf("socket write: %v", err)
	}
}

func readSocketMessage(t *testing.T, c *websocket.Conn, timeout time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("socket read: %v", err)
	}
	return data
}

func socketFrameType(t *testing.T, data []byte) string {
	t.Helper()
	values := providerFrameValues(data)
	return values.Type
}

func configureSocketRule(t *testing.T, env *supplierEnv, rule ResponseModelRule) {
	t.Helper()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode:   ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{rule},
	}); err != nil {
		t.Fatal(err)
	}
}

func socketCreated(id, model string) string {
	return fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":%q,"status":"in_progress"}}`, id, model)
}

func TestProviderSocketRelaysTurnInOrder(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, err := c.Read(ctx)
		if err != nil {
			return
		}
		for _, frame := range []string{
			socketCreated("resp_1", "vendor/big"),
			`{"type":"response.output_text.delta","delta":"ok"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/big","status":"completed"}}`,
		} {
			_ = c.Write(ctx, websocket.MessageText, []byte(frame))
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	want := []string{"response.created", "response.output_text.delta", "response.completed"}
	for i, kind := range want {
		frame := readSocketMessage(t, client, 2*time.Second)
		if got := socketFrameType(t, frame); got != kind {
			t.Fatalf("frame %d type=%q, want %q", i, got, kind)
		}
	}
	if env.rec.count() != 1 {
		t.Fatalf("native upgrades=%d", env.rec.count())
	}
}

func TestProviderSocketRetriesBadTurnAfterInterrupt(t *testing.T) {
	var mu sync.Mutex
	var creates [][]byte
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			unit := providerFrameValues(data)
			switch unit.Type {
			case "response.interrupt":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt.accepted","response_id":"resp_1"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`))
				// A vendor may finish the aborted reply after the incomplete
				// frame; it must not be judged as the retried turn.
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/flash","status":"completed"}}`))
			case "response.create":
				mu.Lock()
				creates = append(creates, append([]byte(nil), data...))
				n := len(creates)
				mu.Unlock()
				if n == 1 {
					_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_1", "vendor/flash")))
					continue
				}
				_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_2", "vendor/big")))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_2","model":"vendor/big","status":"completed"}}`))
			}
		}
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
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	first := readSocketMessage(t, client, 2*time.Second)
	if got := providerFrameValues(first); got.Type != "response.created" || got.ResponseID != "resp_2" {
		t.Fatalf("first delivered frame=%s", first)
	}
	second := readSocketMessage(t, client, 2*time.Second)
	if got := socketFrameType(t, second); got != "response.completed" {
		t.Fatalf("second delivered frame=%s", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(creates) != 2 {
		t.Fatalf("creates=%d, want 2", len(creates))
	}
	if string(creates[0]) != string(creates[1]) {
		t.Fatalf("retry create differs:\n%s\n%s", creates[0], creates[1])
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State == StateFailed {
		t.Fatalf("good retry must leave the target usable: %+v", target)
	}
}

func TestProviderSocketRetryStopsWhenTargetBecomesBlocked(t *testing.T) {
	var env *supplierEnv
	var mu sync.Mutex
	creates := 0
	env = newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if providerFrameValues(data).Type == "response.interrupt" {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt.accepted","response_id":"resp_1"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`))
				continue
			}
			if providerFrameValues(data).Type != "response.create" {
				continue
			}
			mu.Lock()
			creates++
			n := creates
			mu.Unlock()
			if n > 1 {
				_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_2", "vendor/big")))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_2","model":"vendor/big","status":"completed"}}`))
				continue
			}
			// The target did not exist when the turn started. Adding and
			// disabling it before the judged retry must stop the replay.
			if err := env.store.SaveTarget(&Target{Model: "vendor/big", Enabled: true, ManualHold: true}); err != nil {
				t.Errorf("SaveTarget: %v", err)
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_1", "vendor/flash")))
		}
	})
	cfg := mustConfig(t, env.store)
	cfg.GatewayURL = env.vendor.URL
	cfg.GatewayKey = "gw-key"
	cfg.ProviderAttempts = 2
	cfg.ResponseModelMode = ResponseModelOn
	cfg.ResponseRules = []ResponseModelRule{{
		ID: "r-flash", Match: ResponseMatchContains, Pattern: "flash", Action: ResponseActionDisable,
	}}
	if err := env.svc.Configure(cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"big","input":[]}`)
	frame := readSocketMessage(t, client, 3*time.Second)
	if got := providerFrameValues(frame); got.Type != "error" {
		t.Fatalf("expected guard error, got %s", frame)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("blocked target was replayed: creates=%d", creates)
	}
}

func TestProviderSocketTerminalBadIsNeverDelivered(t *testing.T) {
	var mu sync.Mutex
	creates := 0
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			unit := providerFrameValues(data)
			switch unit.Type {
			case "response.interrupt":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt.accepted","response_id":"resp_1"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`))
			case "response.create":
				mu.Lock()
				creates++
				mu.Unlock()
				_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_1", "vendor/flash")))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/flash","status":"completed"}}`))
			}
		}
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
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	frame := readSocketMessage(t, client, 3*time.Second)
	values := providerFrameValues(frame)
	if values.Type != "error" {
		t.Fatalf("expected a guard error frame, got %s", frame)
	}
	var envelope struct {
		Status int `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != http.StatusServiceUnavailable || envelope.Error.Code != "model_guard_blocked" {
		t.Fatalf("envelope=%s", frame)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusInternalError {
		t.Fatalf("close status=%v err=%v", code, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates=%d, want 1", creates)
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State != StateFailed {
		t.Fatal("terminal bad reply must still disable the target")
	}
}

func TestProviderSocketJudgesModelNamedLate(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		_, _, err := c.Read(ctx)
		if err != nil {
			return
		}
		for _, frame := range []string{
			socketCreated("resp_1", "vendor/big"),
			`{"type":"response.output_text.delta","delta":"changing"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/flash","status":"completed"}}`,
		} {
			_ = c.Write(ctx, websocket.MessageText, []byte(frame))
		}
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
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	frame := readSocketMessage(t, client, 3*time.Second)
	values := providerFrameValues(frame)
	if values.Type != "error" {
		t.Fatalf("a late bad model was delivered: %s", frame)
	}
}

func TestProviderSocketNoRetryWhenInterruptRejectedOrMissingID(t *testing.T) {
	cases := []struct {
		name        string
		created     string
		onInterrupt string
	}{
		{
			name:        "interrupt failed",
			created:     socketCreated("resp_1", "vendor/flash"),
			onInterrupt: `{"type":"response.interrupt.failed","response_id":"resp_1"}`,
		},
		{
			name:    "missing response id",
			created: `{"type":"response.created","response":{"model":"vendor/flash"}}`,
		},
		{
			name:        "completed after created",
			created:     socketCreated("resp_1", "vendor/flash"),
			onInterrupt: `{"type":"response.completed","response":{"id":"resp_1","model":"vendor/flash","status":"completed"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			creates := 0
			env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
				for {
					_, data, err := c.Read(ctx)
					if err != nil {
						return
					}
					unit := providerFrameValues(data)
					switch unit.Type {
					case "response.create":
						mu.Lock()
						creates++
						mu.Unlock()
						_ = c.Write(ctx, websocket.MessageText, []byte(tc.created))
						if tc.name == "completed after created" {
							_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/flash","status":"completed"}}`))
						}
					case "response.interrupt":
						if tc.onInterrupt != "" {
							_ = c.Write(ctx, websocket.MessageText, []byte(tc.onInterrupt))
						}
					}
				}
			})
			old := providerSocketInterruptWait
			providerSocketInterruptWait = 150 * time.Millisecond
			t.Cleanup(func() { providerSocketInterruptWait = old })
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
			client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
			writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
			frame := readSocketMessage(t, client, 3*time.Second)
			if got := socketFrameType(t, frame); got != "error" {
				t.Fatalf("frame=%s", frame)
			}
			mu.Lock()
			defer mu.Unlock()
			if creates != 1 {
				t.Fatalf("creates=%d, want 1", creates)
			}
		})
	}
}

func TestProviderSocketClientInterruptStopsRetry(t *testing.T) {
	var mu sync.Mutex
	creates := 0
	createSeen := make(chan struct{})
	interrupted := make(chan struct{})
	var once sync.Once
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			unit := providerFrameValues(data)
			switch unit.Type {
			case "response.create":
				mu.Lock()
				creates++
				mu.Unlock()
				once.Do(func() { close(createSeen) })
				select {
				case <-interrupted:
				case <-time.After(300 * time.Millisecond):
				}
				_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_1", "vendor/flash")))
			case "response.interrupt":
				select {
				case <-interrupted:
				default:
					close(interrupted)
				}
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.interrupt.accepted","response_id":"resp_1"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete"}}`))
			}
		}
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
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	select {
	case <-createSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("native did not receive the create")
	}
	writeSocketJSON(t, client, `{"type":"response.interrupt","response_id":"resp_1"}`)
	for range 3 {
		data := readSocketMessage(t, client, 3*time.Second)
		if kind := socketFrameType(t, data); kind == "error" {
			t.Fatalf("client interrupt must not turn into a guard error: %s", data)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("creates=%d, want 1 (no retry after a client interrupt)", creates)
	}
}

func TestProviderSocketWarmupIsPassthrough(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated("resp_warm", "vendor/big")))
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_warm","model":"vendor/big","status":"completed"}}`))
	})
	env.svc.mu.Lock()
	if err := env.store.SaveTarget(&Target{
		Model: "vendor/big", Enabled: true,
		ResponseRuleMode: ResponseRuleCustom,
		ResponseModelRules: []ResponseModelRule{{
			ID: "r-big", Match: ResponseMatchContains, Pattern: "vendor/big", Action: ResponseActionDisable,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	env.svc.mu.Unlock()
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","generate":false,"input":[]}`)
	if got := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); got != "response.created" {
		t.Fatalf("warmup created type=%q", got)
	}
	if got := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); got != "response.completed" {
		t.Fatalf("warmup completed type=%q", got)
	}
	target, _ := env.store.Target("vendor/big")
	if target == nil || target.State == StateFailed {
		t.Fatal("warmup must not be judged as a reply")
	}
}

func TestProviderSocketLargeSlowMessageStreamsBeforeEnd(t *testing.T) {
	oldWait := providerSocketModelWait
	providerSocketModelWait = 100 * time.Millisecond
	t.Cleanup(func() { providerSocketModelWait = oldWait })

	pad := strings.Repeat("x", 80<<10)
	full := `{"type":"response.output_text.delta","pad":"` + pad + `"}`
	prefix := []byte(full[:70<<10])
	rest := []byte(full[70<<10:])
	paused := make(chan struct{})
	resume := make(chan struct{})
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		w, err := c.Writer(ctx, websocket.MessageText)
		if err != nil {
			return
		}
		_, _ = w.Write(prefix)
		close(paused)
		<-resume
		_, _ = w.Write(rest)
		_ = w.Close()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/big","status":"completed"}}`))
		// A second turn proves the connection was not closed by the boundary.
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_2","model":"vendor/big","status":"completed"}}`))
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	<-paused

	// The 64 KiB cache boundary must hand the message prefix to the client
	// while the native message is still open.
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, reader, err := client.Reader(readCtx)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	buf := make([]byte, 8<<10)
	n, err := reader.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("expected prefix before the message ended, n=%d err=%v", n, err)
	}
	got := &strings.Builder{}
	got.Write(buf[:n])
	close(resume)
	readCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = readCtx2
	for {
		m, err := reader.Read(buf)
		if m > 0 {
			got.Write(buf[:m])
		}
		if err != nil {
			break
		}
	}
	if got.String() != full {
		t.Fatalf("streamed message mismatch: got %d bytes, want %d", got.Len(), len(full))
	}
	if kind := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); kind != "response.completed" {
		t.Fatalf("terminal frame=%q", kind)
	}
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	if kind := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); kind != "response.completed" {
		t.Fatalf("next turn frame=%q", kind)
	}
}

func TestProviderSocketAcceptsLargeClientFrame(t *testing.T) {
	var mu sync.Mutex
	size := 0
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		if _, data, err := c.Read(ctx); err == nil {
			mu.Lock()
			size = len(data)
			mu.Unlock()
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/big","status":"completed"}}`))
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	frame := `{"type":"response.create","model":"vendor/big","pad":"` + strings.Repeat("y", 40<<10) + `","input":[]}`
	writeSocketJSON(t, client, frame)
	if kind := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); kind != "response.completed" {
		t.Fatalf("frame=%q", kind)
	}
	mu.Lock()
	defer mu.Unlock()
	if size != len(frame) {
		t.Fatalf("native got %d bytes, want %d", size, len(frame))
	}
}

func TestProviderSocketOriginPolicy(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/big","status":"completed"}}`))
	})
	target := "ws" + strings.TrimPrefix(env.path("responses", "/responses"), "http")
	header := http.Header{}
	header.Set("Origin", "https://evil.example")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		t.Fatal("cross-origin socket must be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status=%v err=%v", resp, err)
	}

	// The request host is the allowed origin.
	origin := strings.Replace(env.guard.URL, "http://", "http://", 1)
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), origin)
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	if kind := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); kind != "response.completed" {
		t.Fatalf("same-origin turn=%q", kind)
	}
}

func TestProviderSocketBackendFailureIsHTTP(t *testing.T) {
	env := newSupplierEnv(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"native down"}`))
	})
	target := "ws" + strings.TrimPrefix(env.path("responses", "/responses"), "http")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, target, nil)
	if err == nil {
		t.Fatal("a failed backend upgrade must not be accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%v err=%v", resp, err)
	}
}

func TestProviderSocketModeOffClosesNextTurn(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"vendor/big","status":"completed"}}`))
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	if kind := socketFrameType(t, readSocketMessage(t, client, 2*time.Second)); kind != "response.completed" {
		t.Fatalf("first turn=%q", kind)
	}
	env.disableProtection(t)
	writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := client.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Fatalf("close code=%v err=%v", code, err)
	}
}

func TestProviderSocketPreviousResponseIDPassesThrough(t *testing.T) {
	var mu sync.Mutex
	var got []byte
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		if _, data, err := c.Read(ctx); err == nil {
			mu.Lock()
			got = append([]byte(nil), data...)
			mu.Unlock()
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_2","model":"vendor/big","status":"completed"}}`))
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	frame := `{"type":"response.create","model":"vendor/big","previous_response_id":"resp_1","input":[{"type":"message","role":"user","content":"hi"}]}`
	writeSocketJSON(t, client, frame)
	_ = readSocketMessage(t, client, 2*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != frame {
		t.Fatalf("native got %s", got)
	}
}

func TestProviderSocketBinaryFrameCloses1003(t *testing.T) {
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.Read(ctx)
	if code := websocket.CloseStatus(err); code != websocket.StatusUnsupportedData {
		t.Fatalf("close code=%v err=%v", code, err)
	}
}

func TestProviderFrameTypeIgnoresNestedAndSurvivesChunks(t *testing.T) {
	frame := []byte(`{"response":{"output":[{"type":"output_text","text":"ok"}]},"type":"response.completed"}`)
	for split := 1; split <= 7; split++ {
		var scanner providerFrameTypeScanner
		for i := 0; i < len(frame); i += split {
			end := i + split
			if end > len(frame) {
				end = len(frame)
			}
			scanner.Write(frame[i:end])
		}
		if got := scanner.Type(); got != "response.completed" {
			t.Fatalf("split %d type=%q", split, got)
		}
	}
	if got := providerFrameType([]byte(`{"response":{"type":"output_text"}}`)); got != "" {
		t.Fatalf("nested type leaked: %q", got)
	}
}

func TestProviderSocketJudgedTerminalReleasesTurn(t *testing.T) {
	var calls atomic.Int32
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if providerFrameValues(data).Type != "response.create" {
				continue
			}
			id := fmt.Sprintf("resp_%d", calls.Add(1))
			_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated(id, "vendor/big")))
			_ = c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"vendor/big","status":"completed"}}`, id)))
		}
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
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	for turn := 1; turn <= 2; turn++ {
		writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
		for _, want := range []string{"response.created", "response.completed"} {
			if got := socketFrameType(t, readSocketMessage(t, client, 5*time.Second)); got != want {
				t.Fatalf("turn %d frame=%q, want %q", turn, got, want)
			}
		}
	}
}

func TestProviderSocketLargeTerminalReleasesNextTurn(t *testing.T) {
	var calls atomic.Int32
	env := newSocketEnv(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if providerFrameValues(data).Type != "response.create" {
				continue
			}
			id := fmt.Sprintf("resp_%d", calls.Add(1))
			_ = c.Write(ctx, websocket.MessageText, []byte(socketCreated(id, "vendor/big")))
			big := strings.Repeat("x", 9<<20)
			_ = c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"response":{"id":%q,"model":"vendor/big","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]},"type":"response.completed"}`, id, big)))
		}
	})
	client := dialGuardSocket(t, env, env.path("responses", "/responses"), "")
	for turn := 1; turn <= 2; turn++ {
		writeSocketJSON(t, client, `{"type":"response.create","model":"vendor/big","input":[]}`)
		for _, want := range []string{"response.created", "response.completed"} {
			if got := socketFrameType(t, readSocketMessage(t, client, 5*time.Second)); got != want {
				t.Fatalf("turn %d frame=%q, want %q", turn, got, want)
			}
		}
	}
}
