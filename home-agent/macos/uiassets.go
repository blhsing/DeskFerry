//go:build darwin

package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"deskferry/internal/buildinfo"
)

// uiAssets holds the macOS Home control panel and screen viewer. The pages use
// relative URLs so they keep working under the random token path prefix.
//
//go:embed assets
var uiAssets embed.FS

var uiAssetTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
}

// serveUIPage writes an embedded HTML page with {{VERSION}} substituted.
func serveUIPage(w http.ResponseWriter, name string) {
	if !serveUIAsset(w, name) {
		http.Error(w, "page is unavailable", http.StatusInternalServerError)
	}
}

// serveUIStatic serves a flat asset name requested under static/.
func serveUIStatic(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.Contains(name, "/") || !fs.ValidPath(name) || path.Ext(name) == ".html" || !serveUIAsset(w, name) {
		http.NotFound(w, r)
	}
}

func serveUIAsset(w http.ResponseWriter, name string) bool {
	ext := path.Ext(name)
	contentType, ok := uiAssetTypes[ext]
	if !ok {
		return false
	}
	data, err := uiAssets.ReadFile("assets/" + name)
	if err != nil {
		return false
	}
	if ext == ".html" {
		data = []byte(strings.ReplaceAll(string(data), "{{VERSION}}", buildinfo.Version))
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
	return true
}
