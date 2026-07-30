// Package web serves the built dashboard from inside the binary.
package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// The dashboard is not committed, so this may hold nothing but the .gitkeep
// that lets the embed compile. Built reports which.
//
//go:embed all:dist
var dist embed.FS

// Prefix is where the dashboard is mounted. The Astro build is compiled with
// the same base, so asset URLs already carry it.
const Prefix = "/admin"

// Built reports whether the dashboard was compiled into this binary.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

func Handler() (http.Handler, error) {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, err
	}
	if !Built() {
		return notBuiltHandler(), nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := resolve(strings.TrimPrefix(r.URL.Path, Prefix))
		if !ok {
			http.NotFound(w, r)
			return
		}

		body, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}

		if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if strings.HasPrefix(name, "assets/") {
			// Asset names carry a content hash, so they can be cached forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(body)
	}), nil
}

// notBuiltHandler explains the missing dashboard rather than returning a bare
// 404, which would look like a routing bug.
func notBuiltHandler() http.Handler {
	const page = `<!doctype html>
<meta charset="utf-8">
<title>Dashboard not built</title>
<style>
body{font:14px/1.6 ui-sans-serif,system-ui,sans-serif;color:#111417;background:#fff;
     margin:0;display:grid;place-items:center;min-height:100vh;padding:24px}
main{max-width:34rem}
h1{font-size:17px;margin:0 0 8px}
p{color:#4c5457;margin:0 0 14px}
pre{background:#f4f6f6;border:1px solid #e6eaea;border-radius:6px;padding:12px;
    font:12.5px ui-monospace,monospace;overflow-x:auto;margin:0}
</style>
<main>
  <h1>The dashboard was not built into this binary</h1>
  <p>The proxy itself is running normally. Only the admin pages are missing.
     Build the dashboard, then rebuild the binary so it can be embedded:</p>
  <pre>make dashboard &amp;&amp; go build ./cmd/quecto</pre>
</main>`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, Prefix), "/assets/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(page))
	})
}

// resolve maps a request path onto a file in the build, adding the .html the
// static build produced for each page.
func resolve(rel string) (string, bool) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return "index.html", true
	}
	if strings.HasPrefix(rel, "../") {
		return "", false
	}
	if path.Ext(rel) == "" {
		rel += ".html"
	}
	return rel, true
}
