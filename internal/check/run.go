// Package check implements the model-guard backend: probe scheduling, grading,
// gate publishing and the management API with an embedded web console.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Options configures the backend service.
type Options struct {
	DataDir      string
	GatePath     string
	Addr         string
	PluginOwned  bool
	MagpieConfig string
	MagpiePath   string
	Cloudflared  string
}

// StartupInfo is printed on stdout (one JSON line) when started by the plugin
// host, mirroring the handshake of magpie-intelligence-check.
type StartupInfo struct {
	Origin  string `json:"origin"`
	Control string `json:"control"`
	Pid     int    `json:"pid"`
}

// Run starts the backend and blocks until the process is interrupted or the
// HTTP server fails. It is the single entry point used by cmd/model-guard.
func Run(o Options) error {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	if o.PluginOwned {
		// The host owns this stdin pipe; EOF also covers a host killed without cleanup.
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			ownerCancel()
		}()
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := OpenStore(filepath.Join(o.DataDir, "model-guard.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	svc, err := NewService(store, o.GatePath)
	if err != nil {
		return err
	}
	defer svc.Close()
	stopOwner := context.AfterFunc(ownerCtx, svc.cancel)
	defer stopOwner()
	if o.MagpieConfig != "" {
		svc.mu.Lock()
		svc.routes = newGroupController(store, o.MagpieConfig, o.MagpiePath)
		// Keep the console available when reconciliation needs operator attention.
		_ = svc.publishGate()
		svc.mu.Unlock()
	}

	mux := http.NewServeMux()
	access := NewAccess("", "")
	console := NewConsole(access, "", o.DataDir, o.Cloudflared)
	registerAPI(mux, svc, access, console)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// A public readiness probe exposes no management state or credentials.
		w.Header().Set("X-Magpie-Model-Guard", "ready")
		w.WriteHeader(http.StatusNoContent)
	})
	registerConsole(mux, access)
	srv := &http.Server{Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second}

	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", o.Addr, err)
	}
	addr := ln.Addr().String()
	// Normalize to a reachable origin for the plugin host.
	origin := fmt.Sprintf("http://%s", normalizeHost(addr))

	// The tunnel needs the final local origin; set it now that the port is known.
	console.SetLocal(origin)

	var info StartupInfo
	info.Origin = origin
	info.Control = access.SetControl()
	info.Pid = os.Getpid()

	access.mu.Lock()
	access.origins[origin] = true
	access.mu.Unlock()
	if o.PluginOwned {
		b, _ := json.Marshal(info)
		fmt.Println(string(b))
	}

	stopHTTP := context.AfterFunc(ownerCtx, func() { _ = srv.Close() })
	defer stopHTTP()
	defer console.Close()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func normalizeHost(addr string) string {
	if host, port, err := net.SplitHostPort(addr); err == nil {
		switch host {
		case "", "::", "0.0.0.0":
			return net.JoinHostPort("127.0.0.1", port)
		}
	}
	return addr
}

// errJSON writes a JSON error body.
func errJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// SetControl rotates and returns the control token, granting the plugin host
// full API access including console link issuance.
func (a *Access) SetControl() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.control = randomHex(24)
	return a.control
}
