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

// The pages this package serves without the dashboard build, kept as markup
// next to the code rather than as string literals inside it.
//
//go:embed pages/*.html
var pages embed.FS

var (
	notFoundHTML = mustPage("pages/not-found.html")
	notBuiltHTML = mustPage("pages/not-built.html")
)

func mustPage(name string) []byte {
	body, err := pages.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return body
}

// Prefix is where the dashboard is mounted. The Astro build is compiled with
// the same base, so asset URLs already carry it.
const Prefix = "/admin"

// LoginPath is the one page a signed out browser is allowed to reach.
const LoginPath = Prefix + "/login"

const loginPage = "login.html"

// Authenticated reports whether a request may see a dashboard page.
type Authenticated func(*http.Request) bool

// Built reports whether the dashboard was compiled into this binary.
func Built() bool {
	_, err := fs.Stat(dist, "dist/index.html")
	return err == nil
}

// Handler serves the dashboard, sending a browser to the login page before any
// markup goes out rather than letting the page load and bounce itself.
func Handler(authed Authenticated) (http.Handler, error) {
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
			notFoundPage(w, r)
			return
		}

		// Decided before the file is read, so an unknown page cannot tell a
		// signed out visitor whether it exists.
		if page := strings.HasSuffix(name, ".html"); page {
			switch signedIn := authed(r); {
			case !signedIn && name != loginPage:
				redirect(w, r, LoginPath)
				return
			case signedIn && name == loginPage:
				redirect(w, r, Prefix+"/")
				return
			}
		}

		body, err := fs.ReadFile(root, name)
		if err != nil {
			notFoundPage(w, r)
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
			// Which page comes back now depends on the session cookie.
			w.Header().Set("Vary", "Cookie")
		}
		securityHeaders(w)
		w.Write(body)
	}), nil
}

// redirect keeps a cookie dependent answer out of every cache between here and
// the browser.
func redirect(w http.ResponseWriter, r *http.Request, to string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Cookie")
	http.Redirect(w, r, to, http.StatusFound)
}

// NotFoundHandler answers a URL this port does not serve. The port exists to
// be opened in a browser, so its misses are pages, whatever the path was. The
// API mounted beside the dashboard answers its own in JSON.
func NotFoundHandler() http.Handler {
	return http.HandlerFunc(notFoundPage)
}

// notFoundPage answers a dashboard URL that does not exist. This mount serves
// a browser, so it says so in the markup a browser renders.
func notFoundPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	securityHeaders(w)
	w.WriteHeader(http.StatusNotFound)
	w.Write(notFoundHTML)
}

// contentSecurityPolicy locks the dashboard down to its own origin. Scripts are
// all external hashed modules, so no inline script is allowed; styles keep
// unsafe-inline because the build and the live meter set a few style attributes.
const contentSecurityPolicy = "default-src 'self'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'; " +
	"object-src 'none'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; " +
	"connect-src 'self'"

// securityHeaders sets the response hardening the dashboard carries on every
// document. Quecto terminates no TLS, so HSTS is left to whatever fronts it.
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// notBuiltHandler explains the missing dashboard rather than returning a bare
// 404, which would look like a routing bug.
func notBuiltHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, Prefix), "/assets/") {
			notFoundPage(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		securityHeaders(w)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(notBuiltHTML)
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
