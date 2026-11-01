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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/qrcode"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
)

type fakeGateway struct {
	event         tracker.Event
	leads         []tracker.Lead
	pageError     error
	pageHook      bool
	submitError   error
	submission    *tracker.Submission
	created       *tracker.Event
	queryCalls    int
	queryHook     func(*fakeGateway)
	pageCalls     int
	banners       []string
	bannerError   error
	previews      []tracker.LinkPreview
	eventError    error
	deleted       []string
	deleteError   error
	forms         []tracker.Form
	internalForms []tracker.Form
	taken         map[string]bool
	edited        []string
	details       []tracker.EventDetails
	slugs         []string
	editError     error
	submittedTo   string
	endedID       string
}

func (g *fakeGateway) Create(_ context.Context, event tracker.Event) error {
	if g.taken[event.ID] {
		return serviceerror.NewWorkflowExecutionAlreadyStarted("taken", "", "")
	}
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
	if g.eventError != nil {
		return tracker.Event{}, g.eventError
	}
	return g.event, nil
}
func (g *fakeGateway) Submit(_ context.Context, id string, input tracker.Submission) error {
	g.submission = &input
	g.submittedTo = id
	return g.submitError
}
func (g *fakeGateway) End(_ context.Context, id string) (tracker.Event, error) {
	g.endedID = id
	g.event.Status = "ended"
	return g.event, nil
}
func (g *fakeGateway) Delete(_ context.Context, id string) error {
	g.deleted = append(g.deleted, id)
	if g.deleteError != nil {
		return g.deleteError
	}
	now := time.Now().UTC()
	g.event.DeletedAt = &now
	return nil
}
func (g *fakeGateway) Banner(_ context.Context, _ string, banner string) (tracker.Event, error) {
	g.banners = append(g.banners, banner)
	if g.bannerError != nil {
		return tracker.Event{}, g.bannerError
	}
	g.event.QRBanner = banner
	return g.event, nil
}
func (g *fakeGateway) Preview(_ context.Context, _ string, preview tracker.LinkPreview) (tracker.Event, error) {
	g.previews = append(g.previews, preview)
	g.event.Preview = &preview
	return g.event, nil
}
func (g *fakeGateway) Form(_ context.Context, _ string, form tracker.Form) (tracker.Event, error) {
	g.forms = append(g.forms, form)
	g.event.Form = &form
	g.event.Revision++
	return g.event, nil
}
func (g *fakeGateway) InternalOnly(_ context.Context, _ string, form tracker.Form) (tracker.Event, error) {
	g.internalForms = append(g.internalForms, form)
	g.event.Form = &form
	g.event.InternalOnly = true
	g.event.Revision++
	return g.event, nil
}
func (g *fakeGateway) Details(_ context.Context, id string, details tracker.EventDetails) (tracker.Event, error) {
	g.edited = append(g.edited, id)
	g.details = append(g.details, details)
	if g.editError != nil {
		return tracker.Event{}, g.editError
	}
	g.event.Name, g.event.Description, g.event.EndDate = details.Name, details.Description, details.EndDate
	return g.event, nil
}
func (g *fakeGateway) Slug(_ context.Context, id, slug string) (tracker.Event, error) {
	g.edited = append(g.edited, id)
	g.slugs = append(g.slugs, slug)
	if g.editError != nil {
		return tracker.Event{}, g.editError
	}
	g.event.Slug = slug
	return g.event, nil
}

// Participants pages through leads; the cursor is the next index.
func (g *fakeGateway) Participants(_ context.Context, _ string, cursor string, limit int) ([]tracker.Lead, string, error) {
	g.pageCalls++
	if g.queryHook != nil && g.pageHook {
		g.queryHook(g)
	}
	if g.pageError != nil {
		return nil, "", g.pageError
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := min(len(g.leads), start+limit)
	next := ""
	if end < len(g.leads) {
		next = strconv.Itoa(end)
	}
	return g.leads[start:end], next, nil
}
func (g *fakeGateway) Participant(_ context.Context, _ string, email string) (*tracker.Lead, error) {
	for _, lead := range g.leads {
		if lead.Email == email {
			return &lead, nil
		}
	}
	return nil, g.pageError
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
	event := tracker.Event{ID: "demo", Name: "Conference", Description: "Say hello", Status: "open", CreatedBy: "private@temporal.io", Count: 123, Revision: 88}
	g := &fakeGateway{event: event}
	w := request(t, testHandler(t, g), "GET", "/api/events/demo", "", "", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 8 {
		t.Fatalf("public fields = %#v", body)
	}
	for _, key := range []string{"createdBy", "createdAt", "count", "revision", "completesAt", "qrBanner"} {
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

func TestLeadPaginationAndCSV(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	first := make([]tracker.Lead, 101)
	for i := range first {
		first[i] = tracker.Lead{Name: "Person", Role: "Engineer", Email: fmt.Sprintf("person%d@example.com", i), FirstSubmittedAt: now, LastSubmittedAt: now}
	}
	first[0].Name = "  =SUM(1,2)"
	first[0].Reason = "hello,\nworld"
	second := []tracker.Lead{{Name: "Second", Role: "@formula", Email: "second@example.com", FirstSubmittedAt: now, LastSubmittedAt: now}}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Demo", EndDate: "2026-10-07", Count: 102, Revision: 4}, leads: append(first, second...)}
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

func TestChangedSnapshotNeverExportsPartialCSV(t *testing.T) {
	token := validSession
	makeGateway := func() *fakeGateway {
		return &fakeGateway{event: tracker.Event{ID: "demo", Count: 1, Revision: 2}, leads: []tracker.Lead{{Name: "One"}}}
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
	g.pageError = errors.New("store unavailable")
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "event_id") {
		t.Fatalf("partial CSV exported: %d %s", w.Code, w.Body.String())
	}
	g = makeGateway()
	g.event.Count = 2
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/export.csv", "", token, "")
	if w.Code != 409 {
		t.Fatalf("mismatched count exported: %d", w.Code)
	}
	// A submission between reading the event and reading a participant page.
	g = makeGateway()
	g.pageHook = true
	g.queryHook = func(g *fakeGateway) { g.event.Revision++ }
	w = request(t, testHandler(t, g), "GET", "/api/admin/events/demo/participants", "", token, "")
	if w.Code != 409 {
		t.Fatalf("participants changed mid-read were returned: %d", w.Code)
	}
}

func TestCursorRejectsChangedEvent(t *testing.T) {
	event := tracker.Event{ID: "demo", Revision: 4}
	cursor := encodeLeadCursor(leadCursor{EventID: "demo", Revision: 3, Key: "k"})
	if _, err := decodeLeadCursor(cursor, event); !errors.Is(err, errChanged) {
		t.Fatalf("stale cursor accepted: %v", err)
	}
	cursor = encodeLeadCursor(leadCursor{EventID: "other", Revision: 4, Key: "k"})
	if _, err := decodeLeadCursor(cursor, event); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("cross-event cursor accepted: %v", err)
	}
}

func TestTemporalErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
	}{{temporal.NewApplicationError("closed", "Closed"), 410}, {temporal.NewApplicationError("invalid", "Invalid"), 400}, {temporal.NewApplicationError("refreshing", "Refreshing"), 503}, {temporal.NewApplicationError("paused", "Paused"), 503}, {tracker.ErrNotStored, 404}, {serviceerror.NewNotFound("workflow not running"), 503}, {context.DeadlineExceeded, 503}}
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

func TestSubheadingIsSetAtCreationAndCanBeBlank(t *testing.T) {
	const origin = "https://leads.tmprl-demo.cloud"
	blank, custom := "", "Thanks for stopping by"
	for _, tt := range []struct {
		name, body string
		stored     *string
		public     string
	}{
		{"omitted", `{"name":"Summit","endDate":"2099-01-01"}`, nil, tracker.DefaultSubheading},
		{"blank", `{"name":"Summit","endDate":"2099-01-01","subheading":"   "}`, &blank, ""},
		{"custom", `{"name":"Summit","endDate":"2099-01-01","subheading":" Thanks for stopping by "}`, &custom, custom},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGateway{}
			w := request(t, testHandler(t, g), "POST", "/api/admin/events", tt.body, validSession, origin)
			if w.Code != http.StatusCreated || g.created == nil || !reflect.DeepEqual(g.created.Subheading, tt.stored) {
				t.Fatalf("create = %d %s, stored %v", w.Code, w.Body.String(), g.created)
			}
			g.event = *g.created
			w = request(t, testHandler(t, g), "GET", "/api/events/"+g.event.ID, "", "", "")
			var body struct{ Subheading *string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Subheading == nil || *body.Subheading != tt.public {
				t.Fatalf("public subheading = %s", w.Body.String())
			}
		})
	}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}}
	request(t, testHandler(t, g), "POST", "/api/admin/events/demo/details", `{"name":"Renamed","endDate":"2099-01-02","subheading":"  "}`, validSession, origin)
	if len(g.details) != 1 || g.details[0].Subheading == nil || *g.details[0].Subheading != "" {
		t.Fatalf("blank subheading not passed through: %+v", g.details)
	}
	w := request(t, testHandler(t, g), "POST", "/api/admin/events/demo/details", `{"name":"Renamed","endDate":"2099-01-02","subheading":"`+strings.Repeat("x", 121)+`"}`, validSession, origin)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("long subheading = %d", w.Code)
	}
}

func TestPublicEventQRDoesNotRequireAnEmployee(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo"}}
	h := testHandler(t, g)
	w := request(t, h, "GET", "/events/demo/qr.png", "", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(w.Body.String(), "\x89PNG\r\n\x1a\n") {
		t.Fatalf("public QR status %d", w.Code)
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

func TestDeleteRemovesEventFromTheAPI(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference", Status: "open"}}
	h := testHandler(t, g)
	w := request(t, h, "POST", "/api/admin/events/demo/delete", `{}`, "", "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusUnauthorized || len(g.deleted) != 0 {
		t.Fatalf("missing session: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "POST", "/api/admin/events/demo/delete", `{}`, validSession, "")
	if w.Code != http.StatusForbidden || len(g.deleted) != 0 {
		t.Fatalf("missing origin: %d deleted=%v", w.Code, g.deleted)
	}
	w = request(t, h, "POST", "/api/admin/events/demo/delete", `{}`, validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted"`) || len(g.deleted) != 1 || g.deleted[0] != "demo" {
		t.Fatalf("delete: %d %s calls=%v", w.Code, w.Body.String(), g.deleted)
	}
	w = request(t, h, "GET", "/api/events/demo", "", "", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "This event was deleted.") {
		t.Fatalf("public event: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "GET", "/api/admin/events/demo", "", validSession, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("admin event: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "GET", "/api/admin/events", "", validSession, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Conference") {
		t.Fatalf("directory still lists deleted event: %s", w.Body.String())
	}
	w = request(t, h, "POST", "/api/events/demo/participants", `{"name":"Ada","role":"Engineer","email":"ada@example.com","requestId":"01234567-89ab-cdef-0123-456789abcdef"}`, "", "")
	if w.Code != http.StatusNotFound || g.submission != nil {
		t.Fatalf("deleted event accepted a submission: %d %s", w.Code, w.Body.String())
	}
}

func TestDetailsAndSlugEditsUseTheResolvedEvent(t *testing.T) {
	const origin = "https://leads.tmprl-demo.cloud"
	tests := []struct {
		name, path, body, origin string
		status                   string
		want                     int
		edited                   bool
	}{
		{"details through alias", "/api/admin/events/renamed/details", `{"name":"  Renamed  ","description":" Hi ","endDate":"2099-01-02"}`, origin, "open", 200, true},
		{"slug through alias", "/api/admin/events/renamed/slug", `{"slug":"KubeCon 2026"}`, origin, "open", 200, true},
		{"details without origin", "/api/admin/events/demo/details", `{"name":"Renamed","endDate":"2099-01-02"}`, "", "open", 403, false},
		{"blank name", "/api/admin/events/demo/details", `{"name":" ","endDate":"2099-01-02"}`, origin, "open", 400, false},
		{"bad date", "/api/admin/events/demo/details", `{"name":"Renamed","endDate":"tomorrow"}`, origin, "open", 400, false},
		{"reserved slug", "/api/admin/events/demo/slug", `{"slug":"admin"}`, origin, "open", 400, false},
		{"archived details", "/api/admin/events/demo/details", `{"name":"Renamed","endDate":"2099-01-02"}`, origin, "archived", 410, false},
		{"archived slug", "/api/admin/events/demo/slug", `{"slug":"later"}`, origin, "archived", 410, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Conference", EndDate: "2099-01-01", Status: tt.status}}
			w := request(t, testHandler(t, g), "POST", tt.path, tt.body, validSession, tt.origin)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.edited != (len(g.edited) == 1) || tt.edited && g.edited[0] != "demo" {
				t.Fatalf("edited %v", g.edited)
			}
		})
	}
	g := &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}}
	request(t, testHandler(t, g), "POST", "/api/admin/events/demo/details", `{"name":"  Renamed  ","description":" Hi ","endDate":"2099-01-02"}`, validSession, origin)
	if len(g.details) != 1 || g.details[0] != (tracker.EventDetails{Name: "Renamed", Description: "Hi", EndDate: "2099-01-02"}) {
		t.Fatalf("details not normalized: %+v", g.details)
	}
	request(t, testHandler(t, g), "POST", "/api/admin/events/demo/slug", `{"slug":"KubeCon 2026"}`, validSession, origin)
	if len(g.slugs) != 1 || g.slugs[0] != "kubecon-2026" {
		t.Fatalf("slug not normalized: %v", g.slugs)
	}
	g = &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}, editError: temporal.NewApplicationError("that event link is already in use", "Invalid")}
	w := request(t, testHandler(t, g), "POST", "/api/admin/events/demo/slug", `{"slug":"taken"}`, validSession, origin)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "already in use") {
		t.Fatalf("taken slug: %d %s", w.Code, w.Body.String())
	}
}

func TestAliasRequestsReachTheEvent(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo", Slug: "renamed", Status: "open"}}
	h := testHandler(t, g)
	w := request(t, h, "POST", "/api/events/old-tab-link/participants", `{"name":"Ada","role":"Engineer","email":"ada@example.com","requestId":"01234567-89ab-cdef-0123-456789abcdef"}`, "", "")
	if w.Code != 200 || g.submission == nil || g.submittedTo != "demo" {
		t.Fatalf("submit through alias: %d %s, submitted to %q", w.Code, w.Body.String(), g.submittedTo)
	}
	w = request(t, h, "POST", "/api/admin/events/old-tab-link/banner", `{"banner":"Hi"}`, validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != 200 || len(g.banners) != 1 {
		t.Fatalf("banner through alias: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "POST", "/api/admin/events/old-tab-link/end", `{}`, validSession, "https://leads.tmprl-demo.cloud")
	if w.Code != 200 || g.endedID != "demo" {
		t.Fatalf("end through alias: %d %s, ended %q", w.Code, w.Body.String(), g.endedID)
	}
}

func TestQRCodeLinksToTheCurrentSlug(t *testing.T) {
	var encoded []string
	for _, event := range []tracker.Event{{ID: "demo"}, {ID: "demo", Slug: "renamed"}} {
		w := request(t, testHandler(t, &fakeGateway{event: event}), "GET", "/events/old/qr.png", "", "", "")
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		encoded = append(encoded, w.Body.String())
	}
	want, err := qrcode.EncodePNG("https://leads.tmprl-demo.cloud/renamed", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if encoded[0] == encoded[1] || encoded[1] != string(want) {
		t.Fatal("QR code does not follow the current slug")
	}
}

func TestPausedWritesAreTurnedAwayWhileReadsWork(t *testing.T) {
	g := &fakeGateway{event: tracker.Event{ID: "demo", Name: "Demo", Status: "open"}}
	h, err := newHandler(Config{PublicURL: "https://leads.tmprl-demo.cloud", AuthVerifyURL: verifierURL, WritesPaused: true}, g, verifierTransport(func(*http.Request) (*http.Response, error) {
		return verifierResponse(http.StatusNoContent, "google:employee-id", "employee@temporal.io"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://leads.tmprl-demo.cloud"
	writes := []struct{ path, body string }{
		{"/api/events/demo/participants", `{"name":"Ada","role":"Engineer","email":"ada@example.com","requestId":"01234567-89ab-cdef"}`},
		{"/api/admin/events", `{"name":"Conference","endDate":"2099-01-01"}`},
		{"/api/admin/events/demo/end", `{}`},
		{"/api/admin/events/demo/delete", `{}`},
		{"/api/admin/events/demo/banner", `{"banner":"Booth"}`},
		{"/api/admin/events/demo/preview", `{"title":"Hi"}`},
		{"/api/admin/events/demo/form", `{}`},
		{"/api/admin/events/demo/details", `{}`},
		{"/api/admin/events/demo/slug", `{"slug":"renamed"}`},
	}
	for _, write := range writes {
		w := request(t, h, "POST", write.path, write.body, validSession, origin)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "paused") {
			t.Errorf("%s: %d %s", write.path, w.Code, w.Body.String())
		}
	}
	if g.created != nil || g.submission != nil || len(g.deleted)+len(g.banners)+len(g.previews)+len(g.forms)+len(g.details)+len(g.slugs) > 0 || g.endedID != "" {
		t.Fatalf("a paused write reached the gateway: %+v", g)
	}
	for _, path := range []string{"/api/events/demo", "/api/admin/events/demo", "/api/admin/events"} {
		if w := request(t, h, "GET", path, "", validSession, ""); w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
