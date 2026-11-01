package web

import (
	"compress/gzip"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

func TestStaticFilesAreVersionedCachedAndCompressed(t *testing.T) {
	h := testHandler(t, &fakeGateway{})
	page := request(t, h, "GET", "/", "", "", "").Body.String()
	versioned := regexp.MustCompile(`/static/styles\.css\?v=[0-9a-f]{12}`).FindString(page)
	if versioned == "" || !strings.Contains(page, "/static/app.js?v=") || !strings.Contains(page, "/static/temporal-logo.svg?v=") {
		t.Fatalf("page doesn't use versioned assets:\n%s", page)
	}
	r := httptest.NewRequest("GET", versioned, nil)
	r.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("versioned css: %d %v", w.Code, w.Header())
	}
	reader, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	css, _ := io.ReadAll(reader)
	// Stylesheets refer to fonts by their versioned URLs too.
	if !regexp.MustCompile(`/static/fonts/manrope-latin\.woff2\?v=[0-9a-f]{12}`).Match(css) {
		t.Fatal("stylesheet font URLs aren't versioned")
	}
	// Without the current version, the browser revalidates, and an unchanged file is a 304.
	plain := request(t, h, "GET", "/static/styles.css", "", "", "")
	if plain.Header().Get("Cache-Control") != "no-cache" || plain.Header().Get("ETag") == "" {
		t.Fatalf("unversioned css: %v", plain.Header())
	}
	r = httptest.NewRequest("GET", "/static/styles.css", nil)
	r.Header.Set("If-None-Match", plain.Header().Get("ETag"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("revalidation: %d", w.Code)
	}
	if w := request(t, h, "GET", "/static/missing.js", "", "", ""); w.Code != 404 {
		t.Fatalf("missing file: %d", w.Code)
	}
	// Images aren't compressed again; pages and API responses are.
	r = httptest.NewRequest("GET", "/static/temporal-stars.avif", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Content-Type") != "image/avif" {
		t.Fatalf("image: %v", w.Header())
	}
}

func TestAPIResponsesAreCompressedAndTimed(t *testing.T) {
	h := testHandler(t, &fakeGateway{event: tracker.Event{ID: "demo", Name: "Demo", Status: "open"}})
	r := httptest.NewRequest("GET", "/api/admin/events", nil)
	r.AddCookie(&http.Cookie{Name: "temporal_demo_auth", Value: validSession})
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	reader, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Events []tracker.Event }
	if err := json.NewDecoder(reader).Decode(&body); err != nil || len(body.Events) != 1 {
		t.Fatalf("body = %+v, %v", body, err)
	}
	timing := w.Header().Get("Server-Timing")
	if !strings.Contains(timing, `auth;dur=`) || !strings.Contains(timing, "total;dur=") {
		t.Fatalf("Server-Timing = %q", timing)
	}
	// A client that doesn't accept gzip gets plain JSON.
	if w := request(t, h, "GET", "/api/events/demo", "", "", ""); w.Header().Get("Content-Encoding") != "" || !strings.HasPrefix(w.Body.String(), "{") {
		t.Fatalf("plain: %v", w.Header())
	}
}

var bootstrapAttribute = regexp.MustCompile(`data-bootstrap="([^"]*)"`)

func bootstrapOf(t *testing.T, page string) map[string]json.RawMessage {
	t.Helper()
	match := bootstrapAttribute.FindStringSubmatch(page)
	if match == nil {
		t.Fatalf("no bootstrap in page:\n%s", page)
	}
	var bootstrap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &bootstrap); err != nil {
		t.Fatal(err)
	}
	return bootstrap
}

func TestPagesCarryTheirFirstReads(t *testing.T) {
	event := tracker.Event{ID: "demo", Name: "Demo <Conf>", Status: "open", Slug: "renamed", Aliases: []string{"renamed"}}
	g := &fakeGateway{event: event, leads: []tracker.Lead{{Name: "Ada", Email: "ada@example.com"}}}
	h := testHandler(t, g)

	dashboard := bootstrapOf(t, request(t, h, "GET", "/admin", "", validSession, "").Body.String())
	if !strings.Contains(string(dashboard["/api/session"]), `"authenticated":true`) || !strings.Contains(string(dashboard["/api/admin/events"]), `"id":"demo"`) {
		t.Fatalf("dashboard bootstrap = %s", dashboard)
	}
	// Opened through an earlier link: the event by that link, participants by its own ID.
	detail := bootstrapOf(t, request(t, h, "GET", "/admin/events/renamed", "", validSession, "").Body.String())
	if detail["/api/admin/events/renamed"] == nil || !strings.Contains(string(detail["/api/admin/events/demo/participants"]), "ada@example.com") {
		t.Fatalf("detail bootstrap keys = %v", keys(detail))
	}
	// The embedded JSON is attribute-escaped, so event text can't break out of it.
	page := request(t, h, "GET", "/admin/events/demo", "", validSession, "").Body.String()
	if strings.Contains(page, "<Conf>") {
		t.Fatal("event text was embedded unescaped")
	}
	attendee := bootstrapOf(t, request(t, h, "GET", "/renamed", "", "", "").Body.String())
	var public struct{ Name string }
	if json.Unmarshal(attendee["/api/events/renamed"], &public) != nil || public.Name != "Demo <Conf>" || attendee["/api/session"] != nil {
		t.Fatalf("attendee bootstrap = %s", attendee)
	}
	// A read that fails is left for the app to make, so it shows the error as usual.
	internal := &fakeGateway{event: tracker.Event{ID: "staff", Status: "open", InternalOnly: true}}
	if got := bootstrapOf(t, request(t, testHandler(t, internal), "GET", "/staff", "", "", "").Body.String()); len(got) != 0 {
		t.Fatalf("a signed-out internal-only read was embedded: %v", keys(got))
	}
	home := bootstrapOf(t, request(t, h, "GET", "/", "", "", "").Body.String())
	if !strings.Contains(string(home["/api/session"]), `"authenticated":false`) {
		t.Fatalf("home bootstrap = %s", home)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
