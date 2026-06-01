package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// embeddedUI holds the built Web UI bundle (Vite output under uidist/). The
// frontend source lives in web/; `npm run build` writes its output here and the
// result is committed so `go build` works without a Node toolchain (CI and the
// e2e harness are node-free). See web/README.md for the rebuild step.
//
// The `all:` prefix includes files Vite emits with a leading '_' or '.'
// (e.g. hashed chunk names), which the default embed pattern would skip.
//
//go:embed all:uidist
var embeddedUI embed.FS

// uiSubFS roots the embedded filesystem at the uidist/ directory so paths are
// served relative to the bundle root (e.g. "index.html", "assets/...").
func uiSubFS() (fs.FS, error) {
	return fs.Sub(embeddedUI, "uidist")
}

// embeddedUIPresent reports whether a real UI bundle was built into the binary.
// When only the committed placeholder (or nothing) is present the daemon logs a
// warning and still serves what it has — the build never fails on a missing UI.
func embeddedUIPresent(sub fs.FS) bool {
	f, err := sub.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// uiHandler serves the embedded single-page app under the /ui/ prefix.
//
// It is mounted OUTSIDE the auth/audit middleware chain on purpose: the login
// screen and JS bundle must load before the user has a Bearer token, and the
// bundle carries no secrets (the token lives only client-side). Every data API
// call the SPA then makes (/vms, /flocks, ...) still passes through
// authMiddleware unchanged.
//
// Unknown sub-paths that are not asset requests fall back to index.html so that
// client-side routes (e.g. /ui/vms/<id>) survive a page reload.
//
// Note: net/http.FileServerFS landed in Go 1.22; this uses http.FileServer +
// http.FS to stay compatible with the go.mod 1.21 declaration.
func (cp *ControlPlane) uiHandler() http.Handler {
	sub, err := uiSubFS()
	if err != nil {
		slog.Warn("ui: failed to open embedded bundle, serving 404", "err", err)
		return http.NotFoundHandler()
	}
	if !embeddedUIPresent(sub) {
		slog.Warn("ui: no index.html in embedded bundle; run `cd web && npm run build` to populate uidist/")
	}

	fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(sub)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Resolve the path relative to the bundle root.
		rel := strings.TrimPrefix(r.URL.Path, "/ui/")
		rel = path.Clean("/" + rel)[1:] // strip leading slash, clean ../ etc.

		// Serve the bundle root directly.
		if rel == "" {
			r.URL.Path = "/ui/"
			fileServer.ServeHTTP(w, r)
			return
		}

		// If the requested file exists in the bundle, serve it as-is.
		if _, statErr := fs.Stat(sub, rel); statErr == nil {
			fileServer.ServeHTTP(w, r)
			return
		} else if !os.IsNotExist(statErr) {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Missing file: asset requests 404 honestly; everything else is a
		// client-side route → serve index.html so a reload works.
		if isAssetRequest(rel) {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r, sub)
	})
}

// isAssetRequest reports whether a missing path looks like a static asset
// (under assets/ or carrying a file extension) rather than an SPA route.
func isAssetRequest(rel string) bool {
	if strings.HasPrefix(rel, "assets/") {
		return true
	}
	return path.Ext(rel) != ""
}

// serveIndex writes the SPA entry document with a 200 status.
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// rootRedirectOr returns a handler that redirects the bare root path to the UI
// and delegates every other path to the existing API chain. This keeps the API
// catch-all (auth + audit) intact while giving visitors a landing page.
func rootRedirectOr(apiChain http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		apiChain.ServeHTTP(w, r)
	})
}
