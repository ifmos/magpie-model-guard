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
	"strings"
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

const backendAddrKey = "backend_listen_addr"

// BackendAddr returns the persisted management API address, if any.
func (s *Store) BackendAddr() (string, bool, error) {
	var addr string
	ok, err := s.getKV(backendAddrKey, &addr)
	if err != nil || !ok {
		return "", false, err
	}
	addr = strings.TrimSpace(addr)
	return addr, addr != "", nil
}

// SaveBackendAddr remembers the bound management API address.
func (s *Store) SaveBackendAddr(addr string) error { return s.setKV(backendAddrKey, addr) }

// listenBackend binds the management API. The default ephemeral address is
// remembered so provider URLs stay stable across restarts. An explicit address
// wins over the saved one and refreshes it after a successful bind. A saved
// address that is already in use fails startup instead of drifting to a new
// port, because clients were configured against the saved URL.
func listenBackend(store *Store, requested string) (net.Listener, string, error) {
	if requested == "" {
		requested = "127.0.0.1:0"
	}
	saved, ok, err := store.BackendAddr()
	if err != nil {
		// Never overwrite state we could not read; bind the requested address.
		ln, lerr := net.Listen("tcp", requested)
		if lerr != nil {
			return nil, "", fmt.Errorf("listen %s: %w", requested, lerr)
		}
		return ln, ln.Addr().String(), nil
	}
	listenAddr := requested
	explicit := requested != "127.0.0.1:0"
	if !explicit && ok {
		listenAddr = saved
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, "", fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	addr := ln.Addr().String()
	if explicit || !ok || addr != saved {
		if err := store.SaveBackendAddr(addr); err != nil {
			_ = ln.Close()
			return nil, "", fmt.Errorf("save backend address: %w", err)
		}
	}
	return ln, addr, nil
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
	svc.dataDir = o.DataDir
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
	srv := &http.Server{Handler: securityHeaders(registerProvider(mux, svc)), ReadHeaderTimeout: 10 * time.Second}

	ln, addr, err := listenBackend(store, o.Addr)
	if err != nil {
		return err
	}
	// Normalize to a reachable origin for the plugin host.
	origin := fmt.Sprintf("http://%s", normalizeHost(addr))

	// The tunnel needs the final local origin; set it now that the port is known.
	console.SetLocal(origin)
	svc.mu.Lock()
	svc.providerBase = origin + "/provider/v1"
	svc.providerRoot = origin + "/provider"
	svc.providerOrigin = origin
	svc.mu.Unlock()

	var info StartupInfo
	info.Origin = origin
	info.Control = access.SetControl()
	info.Pid = os.Getpid()

	access.mu.Lock()
	access.origins[origin] = true
	access.mu.Unlock()
	if err := svc.ConfigureResponseHook(filepath.Join(o.DataDir, "response-hook.json"), origin, info.Control); err != nil {
		_ = ln.Close()
		return fmt.Errorf("write response hook: %w", err)
	}
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
