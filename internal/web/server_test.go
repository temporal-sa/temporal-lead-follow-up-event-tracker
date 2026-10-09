package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
)

type fakeGateway struct {
	event       tracker.Event
	shards      map[string][]tracker.Lead
	pageError   error
	submitError error
	submission  *tracker.Submission
	created     *tracker.Event
	queryCalls  int
	queryHook   func(*fakeGateway)
	pageCalls   int
	banners     []string
	bannerError error
}

func (g *fakeGateway) Create(_ context.Context, event tracker.Event) error {
	g.created = &event
	return nil
}
func (g *fakeGateway) List(context.Context, string) ([]tracker.Event, string, error) {
	return []tracker.Event{g.event}, "", nil
}
func (g *fakeGateway) Event(context.Context, string) (tracker.Event, error) {
	g.queryCalls++
	if g.queryHook != nil {
		g.queryHook(g)
	}
	return g.event, nil
}
func (g *fakeGateway) Submit(_ context.Context, _ string, input tracker.Submission) error {
	g.submission = &input
	return g.submitError
}
func (g *fakeGateway) IssueFlightPass(_ context.Context, eventID string, submission tracker.Submission) (string, error) {
	return tracker.FlightCode(eventID, submission.RequestID), nil
}
func (g *fakeGateway) End(context.Context, string) (tracker.Event, error) {
	g.event.Status = "ended"
	return g.event, nil
}
func (g *fakeGateway) Banner(_ context.Context, _ string, banner string) (tracker.Event, error) {
	g.banners = append(g.banners, banner)
	if g.bannerError != nil {
		return tracker.Event{}, g.bannerError
	}
	g.event.QRBanner = banner
	return g.event, nil
}
func (g *fakeGateway) Page(_ context.Context, id string, offset, limit int) (tracker.ShardPage, error) {
	g.pageCalls++
	if g.pageError != nil {
		return tracker.ShardPage{}, g.pageError
	}
	leads, ok := g.shards[id]
	if !ok {
		return tracker.ShardPage{}, serviceerror.NewNotFound("missing shard")
	}
	if offset > len(leads) {
		return tracker.ShardPage{Total: len(leads)}, nil
	}
	return tracker.ShardPage{Total: len(leads), Leads: leads[offset:min(len(leads), offset+limit)]}, nil
}
func (g *fakeGateway) Health(context.Context) error { return nil }

func testHandler(t *testing.T, g Gateway) http.Handler {
	t.Helper()
	return testHandlerWithVerifier(t, g, verifierTransport(func(r *http.Request) (*http.Response, error) {
		cookie, err := r.Cookie("temporal_demo_auth")
		if err != nil || cookie.Value != validSession {
			return verifierResponse(http.StatusFound, "", ""), nil
		}
		return verifierResponse(http.StatusNoContent, "google:employee-id", "employee@temporal.io"), nil
	}))
}

func request(t *testing.T, h http.Handler, method, path, body, token, origin string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "temporal_demo_auth", Value: token})
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPublicEventDoesNotLeakEmployeeOrParticipantState(t *testing.T) {
	event := tracker.Event{ID: "demo", Name: "Conference", Description: "Say hello", Status: "open", CreatedBy: "private@temporal.io", Shards: []string{"private-shard"}, Count: 123, Revision: 88, Busy: true}
	g := &fakeGateway{event: event}
	w := request(t, testHandler(t, g), "GET", "/api/events/demo", "", "", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 7 {
		t.Fatalf("public fields = %#v", body)
	}
	for _, key := range []string{"createdBy", "createdAt", "count", "shards", "revision", "busy", "completesAt", "qrBanner"} {
		if _, ok := body[key]; ok {
			t.Errorf("public event leaked %s", key)
		}
	}
	if _, ok := body["form"].(map[string]any); !ok {
		t.Fatalf("public default form missing: %#v", body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("public response may be cached")
	}
}

func TestOriginAndSubmissionValidation(t *testing.T) {
	valid := `{"name":"  Ada  ","role":"  Engineer  ","email":"  ADA@EXAMPLE.COM ","reason":" Talk later ","requestId":"01234567-89ab-cdef-0123-456789abcdef"}`
	tests := []struct {
		name, method, path, body, origin, contentType string
		want                                          int
	}{
		{"normalized public submit", "POST", "/api/events/demo/participants", valid, "https://leads.tmprl-demo.cloud", "application/json", 200},
		{"public API without origin", "POST", "/api/events/demo/participants", valid, "", "application/json", 200},
		{"foreign origin", "POST", "/api/events/demo/participants", valid, "https://evil.example", "application/json", 403},
		{"bad media type", "POST", "/api/events/demo/participants", valid, "", "text/plain", 415},
		{"unknown field", "POST", "/api/events/demo/participants", `{"surprise":true}`, "", "application/json", 400},
		{"oversized body", "POST", "/api/events/demo/participants", `{"name":"` + strings.Repeat("a", 17000) + `"}`, "", "application/json", 400},
		{"invalid email", "POST", "/api/events/demo/participants", strings.Replace(valid, "ADA@EXAMPLE.COM", "invalid", 1), "", "application/json", 400},
		{"missing employee origin", "POST", "/api/admin/events", `{"name":"Conference","endDate":"2099-01-01"}`, "", "application/json", 403},
		{"create event", "POST", "/api/admin/events", `{"name":"Conference","endDate":"2099-01-01"}`, "https://leads.tmprl-demo.cloud", "application/json", 201},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}}
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.contentType)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			r.AddCookie(&http.Cookie{Name: "temporal_demo_auth", Value: validSession})
			w := httptest.NewRecorder()
			testHandler(t, g).ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.name == "normalized public submit" && (g.submission == nil || g.submission.Email != "ada@example.com" || g.submission.Name != "Ada" || g.submission.Role != "Engineer" || g.submission.Reason != "Talk later") {
				t.Fatalf("submission not normalized: %#v", g.submission)
			}
			if tt.name == "create event" && (g.created == nil || g.created.CreatedBy != "employee@temporal.io" || g.created.ClosesAt.Format(time.RFC3339) != "2099-01-02T00:00:00Z") {
				t.Fatalf("event = %#v", g.created)
			}
		})
	}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Status: "ended"}, submitError: temporal.NewApplicationError("event closed", "Closed")}
	w := request(t, testHandler(t, g), "POST", "/api/events/demo/participants", valid, "", "")
	if w.Code != 410 {
		t.Fatal("ended event accepted submission")
	}
	g.submitError = nil // The workflow may acknowledge a receipt without writing new data.
	w = request(t, testHandler(t, g), "POST", "/api/events/demo/participants", valid, "", "")
	if w.Code != 200 {
		t.Fatal("ended event must permit acknowledgement of a saved receipt")
	}
}

func TestAdminPagesRedirectAndSessionUsesCatalogReturnTo(t *testing.T) {
	h := testHandler(t, &fakeGateway{})
	w := request(t, h, "GET", "/admin/events/demo/qr", "", "", "")
	if w.Code != 302 {
		t.Fatalf("status = %d", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Host != "catalog.tmprl-demo.cloud" || location.Path != "/auth/login" || location.Query().Get("return_to") != "https://leads.tmprl-demo.cloud/admin" {
		t.Fatalf("login redirect = %s", location)
	}
	w = request(t, h, "GET", "/api/session", "", "", "")
	var session struct {
		Authenticated bool   `json:"authenticated"`
		LoginURL      string `json:"loginUrl"`
	}
	if json.Unmarshal(w.Body.Bytes(), &session) != nil || session.Authenticated || session.LoginURL != location.String() {
		t.Fatalf("session = %s", w.Body.String())
	}
}

func TestLeadPaginationAndCSVAcrossShards(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	first := make([]tracker.Lead, 101)
	for i := range first {
		first[i] = tracker.Lead{Name: "Person", Role: "Engineer", Email: fmt.Sprintf("person%d@example.com", i), FirstSubmittedAt: now, LastSubmittedAt: now}
	}
	first[0].Name = "  =SUM(1,2)"
	first[0].Reason = "hello,\nworld"
	second := []tracker.Lead{{Name: "Second", Role: "@formula", Email: "second@example.com", FirstSubmittedAt: now, LastSubmittedAt: now}}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Demo", EndDate: "2026-10-07", Count: 102, Revision: 4, Shards: []string{"s0", "s1"}}, shards: map[string][]tracker.Lead{"s0": first, "s1": second}}
	h, token := testHandler(t, g), validSession
	w := request(t, h, "GET", "/api/admin/events/demo/participants", "", token, "")
	var page struct {
		Leads      []tracker.Lead `json:"leads"`
		NextCursor string         `json:"nextCursor"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Leads) != 100 || page.NextCursor == "" {
		t.Fatalf("first page = %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "GET", "/api/admin/events/demo/participants?cursor="+page.NextCursor, "", token, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Leads) != 2 || page.NextCursor != "" {
		t.Fatalf("last page = %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 103 || rows[1][3] != "'  =SUM(1,2)" || rows[1][6] != "hello,\nworld" || rows[102][4] != "'@formula" || rows[1][7] != "2026-10-07T12:00:00Z" {
		t.Fatalf("CSV incorrect rows: first=%q last=%q", rows[1], rows[len(rows)-1])
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "event-demo-leads.csv") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("export headers missing")
	}
}

func TestChangedSnapshotAndMissingShardNeverExportPartialCSV(t *testing.T) {
	token := validSession
	makeGateway := func() *fakeGateway {
		return &fakeGateway{event: tracker.Event{ID: "demo", Count: 1, Revision: 2, Shards: []string{"s0"}}, shards: map[string][]tracker.Lead{"s0": {{Name: "One"}}}}
	}
	g := makeGateway()
	g.queryHook = func(g *fakeGateway) {
		if g.queryCalls == 2 {
			g.event.Revision++
		}
	}
	w := request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 200 || g.queryCalls != 4 {
		t.Fatalf("changed export did not retry: %d calls=%d", w.Code, g.queryCalls)
	}
	g = makeGateway()
	g.queryHook = func(g *fakeGateway) { g.event.Revision++ }
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 409 || strings.Contains(w.Body.String(), "event_id") {
		t.Fatalf("unstable CSV exported: %d %s", w.Code, w.Body.String())
	}
	g = makeGateway()
	g.pageError = serviceerror.NewNotFound("retention")
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 410 || strings.Contains(w.Body.String(), "event_id") {
		t.Fatalf("partial CSV exported: %d %s", w.Code, w.Body.String())
	}
	g = makeGateway()
	g.event.Count = 2
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 409 {
		t.Fatalf("mismatched count exported: %d", w.Code)
	}
	g = makeGateway()
	g.event.Count = 2
	g.shards["s0"] = append(g.shards["s0"], g.shards["s0"][0])
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 409 {
		t.Fatalf("duplicated participant exported: %d", w.Code)
	}
	g = makeGateway()
	g.event.Busy = true
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/participants", "", token, "")
	if w.Code != 409 || g.pageCalls != 0 {
		t.Fatal("read while mutation in progress")
	}
}

func TestCursorRejectsChangedEvent(t *testing.T) {
	event := tracker.Event{ID: "demo", Revision: 4, Shards: []string{"s0"}}
	cursor := encodeLeadCursor(leadCursor{EventID: "demo", Revision: 3})
	if _, err := decodeLeadCursor(cursor, event); !errors.Is(err, errChanged) {
		t.Fatalf("stale cursor accepted: %v", err)
	}
	cursor = encodeLeadCursor(leadCursor{EventID: "other", Revision: 4})
	if _, err := decodeLeadCursor(cursor, event); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("cross-event cursor accepted: %v", err)
	}
}

func TestTemporalErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{{temporal.NewApplicationError("closed", "Closed"), 410}, {temporal.NewApplicationError("invalid", "Invalid"), 400}, {temporal.NewApplicationError("refreshing", "Refreshing"), 503}, {serviceerror.NewNotFound("missing"), 404}, {context.DeadlineExceeded, 503}}
	for _, tt := range tests {
		w := httptest.NewRecorder()
		handleError(w, tt.err)
		if w.Code != tt.status {
			t.Errorf("%v -> %d, want %d", tt.err, w.Code, tt.status)
		}
	}
}

func TestLocalAuthMustStayOnLoopback(t *testing.T) {
	_, err := New(Config{PublicURL: "https://public.example", DevAuthEmail: "employee@temporal.io"}, &fakeGateway{})
	if err == nil {
		t.Fatal("public auth bypass accepted")
	}
	h, err := New(Config{PublicURL: "http://127.0.0.1:8080", DevAuthEmail: "employee@temporal.io"}, &fakeGateway{})
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, h, "GET", "/api/admin/events", "", "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestQRIsEmployeeOnlyAndPNGGenerated(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo"}}
	h := testHandler(t, g)
	w := request(t, h, "GET", "/admin/events/demo/qr.png", "", "", "")
	if w.Code != 302 {
		t.Fatal("QR image publicly accessible")
	}
	w = request(t, h, "GET", "/admin/events/demo/qr.png", "", validSession, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Body.String(), "\x89PNG\r\n\x1a\n") {
		t.Fatalf("invalid QR PNG: %d", w.Code)
	}
	w = request(t, h, "GET", "/admin/events/demo/qr", "", validSession, "")
	body, _ := io.ReadAll(w.Body)
	if w.Code != 200 || !strings.Contains(string(body), `src="/admin/events/demo/qr.png"`) {
		t.Fatalf("QR page missing image: %s", body)
	}
}
