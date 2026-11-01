package web

import (
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// timings collects how long a request spent on each dependency, for its
// Server-Timing header: browser developer tools show the breakdown per request.
type timings struct {
	mu      sync.Mutex
	entries map[string]*timing
}

type timing struct {
	total time.Duration
	calls int
}

type timingsKey struct{}

// timed starts timing one call to a dependency (auth, store, or temporal) and
// returns the function that stops it.
func timed(ctx context.Context, name string) func() {
	t, _ := ctx.Value(timingsKey{}).(*timings)
	if t == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		entry := t.entries[name]
		if entry == nil {
			entry = &timing{}
			t.entries[name] = entry
		}
		entry.total += time.Since(start)
		entry.calls++
	}
}

func (t *timings) header(total time.Duration) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.entries))
	for name := range t.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	metrics := make([]string, 0, len(names)+1)
	for _, name := range names {
		entry := t.entries[name]
		metrics = append(metrics, fmt.Sprintf(`%s;dur=%.1f;desc="%d calls"`, name, float64(entry.total.Microseconds())/1000, entry.calls))
	}
	return strings.Join(append(metrics, fmt.Sprintf("total;dur=%.1f", float64(total.Microseconds())/1000)), ", ")
}

// responseWriter adds Server-Timing when the status is written, and gzips a
// compressible body when the client accepts it and the handler hasn't encoded it.
type responseWriter struct {
	http.ResponseWriter
	timings     *timings
	start       time.Time
	acceptsGzip bool
	gzip        *gzip.Writer
	wroteHeader bool
}

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	header := w.Header()
	header.Set("Server-Timing", w.timings.header(time.Since(w.start)))
	if w.acceptsGzip && status != http.StatusNoContent && status != http.StatusNotModified &&
		header.Get("Content-Encoding") == "" && compressible(header.Get("Content-Type")) {
		header.Set("Content-Encoding", "gzip")
		header.Add("Vary", "Accept-Encoding")
		header.Del("Content-Length")
		w.gzip = gzipWriters.Get().(*gzip.Writer)
		w.gzip.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		return w.gzip.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseWriter) finish() {
	if w.gzip != nil {
		_ = w.gzip.Close()
		gzipWriters.Put(w.gzip)
		w.gzip = nil
	}
}
