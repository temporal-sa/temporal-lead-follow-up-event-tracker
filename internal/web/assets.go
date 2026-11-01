package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// staticAssets serves the embedded files under versioned URLs. Every page and
// stylesheet refers to a file as /static/<name>?v=<content hash>, so a browser
// can keep a file for a year and still sees a new version the moment it changes.
// Text files are gzipped once, at startup.
type staticAssets struct {
	files map[string]*staticFile
}

type staticFile struct {
	body, gzipped []byte
	contentType   string
	version       string
}

var staticReference = regexp.MustCompile(`/static/([A-Za-z0-9._/-]+)`)

func loadStaticAssets() (*staticAssets, error) {
	a := &staticAssets{files: map[string]*staticFile{}}
	var text []string
	err := fs.WalkDir(assets, "static", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := assets.ReadFile(name)
		if err != nil {
			return err
		}
		name = strings.TrimPrefix(name, "static/")
		contentType := mime.TypeByExtension(path.Ext(name))
		if path.Ext(name) == ".avif" {
			contentType = "image/avif"
		}
		a.files[name] = &staticFile{body: body, contentType: contentType}
		// Stylesheets and scripts refer to other files, so they are versioned
		// after those files, with the references rewritten.
		if ext := path.Ext(name); ext == ".css" || ext == ".js" {
			text = append(text, name)
		} else {
			a.files[name].finish()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, name := range text {
		file := a.files[name]
		file.body = []byte(a.Rewrite(string(file.body)))
		file.finish()
	}
	return a, nil
}

func (f *staticFile) finish() {
	sum := sha256.Sum256(f.body)
	f.version = hex.EncodeToString(sum[:6])
	if !compressible(f.contentType) {
		return
	}
	var buffer bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	_, _ = writer.Write(f.body)
	_ = writer.Close()
	if buffer.Len() < len(f.body) {
		f.gzipped = buffer.Bytes()
	}
}

// URL returns the versioned URL of an embedded file.
func (a *staticAssets) URL(name string) string {
	file, ok := a.files[name]
	if !ok {
		return "/static/" + name
	}
	return "/static/" + name + "?v=" + file.version
}

// Rewrite versions every /static/ reference in text that names an embedded file.
func (a *staticAssets) Rewrite(text string) string {
	return staticReference.ReplaceAllStringFunc(text, func(reference string) string {
		name := strings.TrimPrefix(reference, "/static/")
		if _, ok := a.files[name]; !ok {
			return reference
		}
		return a.URL(name)
	})
}

func (a *staticAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, ok := a.files[strings.TrimPrefix(r.URL.Path, "/static/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := `"` + file.version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	// Only the current version is immutable; an old or missing version is revalidated.
	if r.URL.Query().Get("v") == file.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if file.contentType != "" {
		w.Header().Set("Content-Type", file.contentType)
	}
	body := file.body
	if file.gzipped != nil && acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		body = file.gzipped
	}
	_, _ = w.Write(body)
}

func compressible(contentType string) bool {
	contentType, _, _ = strings.Cut(contentType, ";")
	switch strings.TrimSpace(contentType) {
	case "text/css", "text/html", "text/javascript", "application/javascript", "application/json", "image/svg+xml", "text/csv", "text/plain":
		return true
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, encoding := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(encoding), ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") && strings.ReplaceAll(params, " ", "") != "q=0" {
			return true
		}
	}
	return false
}
