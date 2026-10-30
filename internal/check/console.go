package check

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html web/app.js web/style.css
var consoleFS embed.FS

// registerConsole serves the embedded web console at /.
func registerConsole(mux *http.ServeMux) {
	sub, err := fs.Sub(consoleFS, "web")
	if err != nil {
		panic(err) // embed error at build time; unreachable
	}
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
