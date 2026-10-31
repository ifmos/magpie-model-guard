package check

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestQuickTunnelLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cloudflared")
	script := "#!/bin/sh\nprintf 'https://fixture-console.trycloudflare.com\\nRegistered tunnel connection\\n' >&2\nexec sleep 300\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a := NewAccess("control", "http://127.0.0.1:1234")
	c := NewConsole(a, "http://127.0.0.1:1234", dir, binary)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	link, err := c.Open(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "https://fixture-console.trycloudflare.com/open/") {
		t.Fatal(link)
	}
	ticket := strings.Split(link, "/open/")[1]
	session, ok := a.Exchange(ticket)
	if !ok {
		t.Fatal("remote ticket failed")
	}
	second, err := c.Open(ctx, true)
	if err != nil || second == link {
		t.Fatal("reopening reused ticket")
	}
	c.Close()
	if a.Session(session) {
		t.Fatal("remote close kept session")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		count := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "tunnel-") {
				count++
			}
		}
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("temporary tunnel configuration not removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestQuickTunnelFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "cloudflared")
	os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0700)
	a := NewAccess("control", "http://127.0.0.1:1234")
	c := NewConsole(a, "http://127.0.0.1:1234", dir, binary)
	if _, err := c.Open(context.Background(), true); err == nil {
		t.Fatal("failed tunnel accepted")
	}
	c.Close()
	if a.Origin("https://fixture-console.trycloudflare.com") {
		t.Fatal("failed tunnel leaked an origin")
	}
}

func TestConsoleTicketExpiryAndRemoteReset(t *testing.T) {
	a := NewAccess("control", "http://127.0.0.1:5555")
	ticket := a.Ticket()
	session, ok := a.Exchange(ticket)
	if !ok || !a.Session(session) {
		t.Fatal("ticket exchange failed")
	}
	if _, ok := a.Exchange(ticket); ok {
		t.Fatal("ticket reused")
	}
	expired := a.Ticket()
	a.mu.Lock()
	a.tickets[expired] = time.Now().Add(-time.Second)
	a.mu.Unlock()
	if _, ok := a.Exchange(expired); ok {
		t.Fatal("expired ticket was accepted")
	}
	a.SetRemote("https://temporary-test.trycloudflare.com")
	if a.Session(session) {
		t.Fatal("session survived a tunnel change")
	}
}

func TestConsoleBrowserLoginAndOrigin(t *testing.T) {
	svc, _, _ := newTestService(t, nil)
	access := NewAccess("control", "http://127.0.0.1:5555")
	access.SetRemote("https://temporary-test.trycloudflare.com")
	mux := http.NewServeMux()
	registerAPI(mux, svc, access, NewConsole(access, "http://127.0.0.1:5555", t.TempDir(), ""))
	registerConsole(mux, access)
	origin := "https://temporary-test.trycloudflare.com"

	ticket := access.Ticket()
	r := httptest.NewRequest("GET", origin+"/open/"+ticket, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].SameSite != http.SameSiteLaxMode || !cookies[0].HttpOnly {
		t.Fatalf("login cookie = %+v", cookies)
	}

	// Session cookie must grant API access; a wrong-origin ticket must not.
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Header.Set("Cookie", "mg_session="+cookies[0].Value)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session api: %d", rec.Code)
	}

	other := access.Ticket()
	req = httptest.NewRequest("GET", origin+"/open/"+other, nil)
	req.Host = "untrusted.example.com"
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", rec.Code)
	}

	// Console issuance is reserved for the plugin control token.
	open := httptest.NewRequest("POST", "/api/console", strings.NewReader(`{"remote":false}`))
	open.Header.Set("Cookie", "mg_session="+cookies[0].Value)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, open)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("session console issue: %d", rec.Code)
	}
	open = httptest.NewRequest("POST", "/api/console", strings.NewReader(`{"remote":false}`))
	open.Header.Set("Authorization", "Bearer control")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, open)
	if rec.Code != http.StatusOK {
		t.Fatalf("control console issue: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.URL, "http://127.0.0.1:5555/open/") {
		t.Fatalf("local link = %q", out.URL)
	}
}
