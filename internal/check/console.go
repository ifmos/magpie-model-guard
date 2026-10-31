package check

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html web/app.js web/style.css
var consoleFS embed.FS

// registerConsole serves the embedded web console at / and the one-time
// ticket exchange at /open/<ticket>.
func registerConsole(mux *http.ServeMux, access *Access) {
	sub, err := fs.Sub(consoleFS, "web")
	if err != nil {
		panic(err) // embed error at build time; unreachable
	}
	mux.HandleFunc("GET /open/", func(w http.ResponseWriter, r *http.Request) {
		if !access.Origin("http://"+r.Host) && !access.Origin("https://"+r.Host) {
			http.Error(w, "管理域名不匹配", http.StatusForbidden)
			return
		}
		session, ok := access.Exchange(strings.TrimPrefix(r.URL.Path, "/open/"))
		if !ok {
			http.Error(w, "管理链接已失效，请重新打开入口", http.StatusUnauthorized)
			return
		}
		secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
		http.SetCookie(w, &http.Cookie{Name: "mg_session", Value: session, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 1800})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.Handle("GET /", http.StripPrefix("/", consoleHandler{sub: sub, files: http.FileServer(http.FS(sub))}))
}

// consoleHandler serves embedded files; unknown paths fall back to the SPA
// entry (index.html).
type consoleHandler struct {
	sub   fs.FS
	files http.Handler
}

func (h consoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if _, err := fs.Stat(h.sub, name); err != nil {
		index, rerr := fs.ReadFile(h.sub, "index.html")
		if rerr != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
		return
	}
	h.files.ServeHTTP(w, r)
}

// securityHeaders sets conservative response headers for the console.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' blob:; font-src 'self'; frame-ancestors 'none'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}
