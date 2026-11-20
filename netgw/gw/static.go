package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Mirrors web/serve.py: Range (via http.ServeContent), wasm MIME, .br/.gz siblings, cache headers.
var mimeTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".wasm": "application/wasm",
	".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon",
	".txt": "text/plain; charset=utf-8", ".map": "application/json",
	".gz": "application/gzip", ".br": "application/octet-stream",
}

var noCacheExt = map[string]bool{".html": true, ".htm": true, ".js": true, ".mjs": true, ".wasm": true, ".json": true}
var immutableRE = regexp.MustCompile(`\.[0-9a-f]{8}\.wasm(\.gz|\.br)?$`)

var precompressed = []struct{ coding, suffix string }{{"br", ".br"}, {"gzip", ".gz"}}

type static struct{ root string }

func (s static) resolve(urlPath string) string {
	if strings.ContainsRune(urlPath, 0) {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(urlPath, "/") {
		if p == ".." || strings.Contains(p, `\`) {
			return ""
		}
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	full := filepath.Join(append([]string{s.root}, parts...)...)
	if st, err := os.Stat(full); err == nil && st.IsDir() {
		full = filepath.Join(full, "index.html")
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return ""
	}
	rootReal, _ := filepath.EvalSymlinks(s.root)
	if real != rootReal && !strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
		return ""
	}
	if st, err := os.Stat(real); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return real
}

func acceptable(header, coding string) bool {
	q := map[string]float64{}
	for _, it := range strings.Split(header, ",") {
		tok, params, _ := strings.Cut(it, ";")
		tok = strings.ToLower(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		v := 1.0
		if i := strings.Index(params, "q="); i >= 0 {
			v, _ = strconv.ParseFloat(strings.TrimSpace(params[i+2:]), 64)
		}
		q[tok] = v
	}
	v, ok := q[coding]
	if !ok && coding == "gzip" {
		v, ok = q["x-gzip"]
	}
	if !ok {
		v = q["*"]
	}
	return v > 0
}

func (s static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodOptions:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	path := s.resolve(r.URL.Path)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	file := path
	st, err := os.Stat(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	coding, siblings := "", false
	rootReal, _ := filepath.EvalSymlinks(s.root)
	for _, p := range precompressed {
		sst, err := os.Stat(path + p.suffix)
		if err != nil || !sst.Mode().IsRegular() {
			continue
		}
		if real, err := filepath.EvalSymlinks(path + p.suffix); err != nil || !strings.HasPrefix(real, rootReal+string(os.PathSeparator)) {
			continue
		}
		if sst.ModTime().Add(2e9).Before(st.ModTime()) {
			continue // stale
		}
		siblings = true
		if coding == "" && r.Header.Get("Range") == "" && acceptable(r.Header.Get("Accept-Encoding"), p.coding) {
			coding, file, st = p.coding, path+p.suffix, sst
		}
	}
	f, err := os.Open(file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h := w.Header()
	ext := strings.ToLower(filepath.Ext(path))
	ct := mimeTypes[ext]
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	if immutableRE.MatchString(path) {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if noCacheExt[ext] || strings.HasPrefix(path, "/disc/") || strings.Contains(path, "/disc/") {
		// disc files are patched in place (UI text); revalidate so a stale heuristic-cache copy never wins
		h.Set("Cache-Control", "no-cache")
	}
	if siblings {
		h.Set("Vary", "Accept-Encoding")
	}
	if coding != "" {
		h.Set("Content-Encoding", coding)
		h.Set("Content-Length", strconv.FormatInt(st.Size(), 10)) // ServeContent skips it when Content-Encoding is set
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}
