package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/qrcode"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
)

//go:embed static/*
var assets embed.FS

type Config struct {
	PublicURL string
	// AuthBaseURL and AuthVerifyURL are the tmprl-demo.cloud catalog's sign-in
	// (temporal_demo_auth cookie, 204 from the verifier). They apply only while
	// PlatformVerifyURL is unset.
	AuthBaseURL   string
	AuthVerifyURL string
	// PlatformVerifyURL, PlatformCookie and PlatformLoginURL are the field demo
	// platform's sign-in (FIELDKIT_AUTH_VERIFY_URL, FIELDKIT_AUTH_COOKIE,
	// FIELDKIT_LOGIN_URL). A set PlatformVerifyURL replaces the catalog
	// verifier; a set PlatformLoginURL replaces the catalog login link.
	// PlatformCookie names the session cookie for diagnostics only: the whole
	// Cookie header is forwarded.
	PlatformVerifyURL string
	PlatformCookie    string
	PlatformLoginURL  string
	// TrustIdentityHeaders accepts X-Temporal-Auth-Email/Name on the request
	// as the signed-in employee. Only safe behind an edge that removes
	// client-sent X-Temporal-Auth-* headers on every route and sets them only
	// for a valid session, as the field demo platform does.
	TrustIdentityHeaders bool
	DevAuthEmail         string
	// WritesPaused turns away every change during the storage cut-over; reads keep working.
	WritesPaused bool
}

type server struct {
	config       Config
	gateway      Gateway
	publicOrigin *url.URL
	authClient   *http.Client
	sessions     *sessionCache
	static       *staticAssets
	mux          *http.ServeMux
	indexPage    *template.Template
	qrDisplay    *template.Template
}

func New(config Config, gateway Gateway) (http.Handler, error) {
	return newHandler(config, gateway, http.DefaultTransport)
}

func newHandler(config Config, gateway Gateway, authTransport http.RoundTripper) (http.Handler, error) {
	config.PublicURL = strings.TrimRight(config.PublicURL, "/")
	public, err := url.Parse(config.PublicURL)
	if err != nil || public.Host == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" || public.Path != "" || (public.Scheme != "https" && public.Scheme != "http") {
		return nil, errors.New("PUBLIC_URL must be an HTTP(S) origin without a path")
	}
	if len(config.PublicURL) > 173 {
		return nil, errors.New("PUBLIC_URL is too long for event QR codes")
	}
	if config.AuthBaseURL == "" {
		config.AuthBaseURL = "https://catalog.tmprl-demo.cloud"
	}
	config.AuthBaseURL = strings.TrimRight(config.AuthBaseURL, "/")
	authBase, err := url.Parse(config.AuthBaseURL)
	if err != nil || authBase.Host == "" || authBase.User != nil || authBase.RawQuery != "" || authBase.Fragment != "" || authBase.Path != "" || (authBase.Scheme != "https" && authBase.Scheme != "http") {
		return nil, errors.New("AUTH_BASE_URL must be an HTTP(S) origin without a path")
	}
	if config.AuthVerifyURL == "" {
		config.AuthVerifyURL = "https://catalog.tmprl-demo.cloud/_auth/verify"
	}
	authVerify, err := url.Parse(config.AuthVerifyURL)
	if err != nil || authVerify.Host == "" || authVerify.User != nil || authVerify.RawQuery != "" || authVerify.Fragment != "" || (authVerify.Scheme != "https" && authVerify.Scheme != "http") {
		return nil, errors.New("AUTH_VERIFY_URL must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if config.PlatformVerifyURL != "" {
		u, err := url.Parse(config.PlatformVerifyURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("FIELDKIT_AUTH_VERIFY_URL must be an HTTP(S) URL without credentials, query, or fragment")
		}
	}
	if config.PlatformLoginURL != "" {
		u, err := url.Parse(config.PlatformLoginURL)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("FIELDKIT_LOGIN_URL must be an HTTP(S) URL without credentials or fragment")
		}
	}
	if config.DevAuthEmail != "" {
		ip := net.ParseIP(public.Hostname())
		if public.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("DEV_AUTH_EMAIL requires a loopback PUBLIC_URL")
		}
		if !employeeEmail(config.DevAuthEmail) {
			return nil, errors.New("DEV_AUTH_EMAIL must be a temporal.io employee email")
		}
	}
	if gateway == nil {
		return nil, errors.New("Temporal gateway is required")
	}
	static, err := loadStaticAssets()
	if err != nil {
		return nil, err
	}
	index, err := assets.ReadFile("static/index.html")
	if err != nil {
		return nil, err
	}
	s := &server{
		config: config, gateway: gateway, publicOrigin: public,
		sessions: newSessionCache(), static: static,
		indexPage: template.Must(template.New("index").Parse(static.Rewrite(string(index)))),
		qrDisplay: template.Must(template.New("qr").Parse(static.Rewrite(qrHTML))),
		authClient: &http.Client{
			Transport: authTransport,
			Timeout:   authTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	mux := http.NewServeMux()
	s.mux = mux
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/events/{id}", s.publicEvent)
	mux.HandleFunc("POST /api/events/{id}/participants", s.writes(s.submit))
	mux.HandleFunc("GET /api/admin/events", s.admin(s.listEvents))
	mux.HandleFunc("POST /api/admin/events", s.admin(s.writes(s.createEvent)))
	mux.HandleFunc("GET /api/admin/events/{id}", s.admin(s.adminEvent))
	mux.HandleFunc("POST /api/admin/events/{id}/end", s.admin(s.writes(s.endEvent)))
	mux.HandleFunc("POST /api/admin/events/{id}/delete", s.admin(s.writes(s.deleteEvent)))
	mux.HandleFunc("POST /api/admin/events/{id}/banner", s.admin(s.writes(s.banner)))
	mux.HandleFunc("POST /api/admin/events/{id}/preview", s.admin(s.writes(s.preview)))
	mux.HandleFunc("POST /api/admin/events/{id}/form", s.admin(s.writes(s.form)))
	mux.HandleFunc("POST /api/admin/events/{id}/details", s.admin(s.writes(s.details)))
	mux.HandleFunc("POST /api/admin/events/{id}/slug", s.admin(s.writes(s.slug)))
	mux.HandleFunc("GET /api/admin/events/{id}/participants", s.admin(s.participants))
	mux.HandleFunc("GET /api/admin/events/{id}/export.csv", s.admin(s.export))
	mux.HandleFunc("GET /admin/events/{id}/qr", s.admin(s.qrPage))
	mux.HandleFunc("GET /admin/events/{id}/qr.png", s.admin(s.qrImage))
	mux.HandleFunc("GET /admin", s.admin(s.dashboardPage))
	mux.HandleFunc("GET /admin/events/{id}", s.admin(s.eventDetailPage))
	mux.HandleFunc("GET /events/{id}/qr.png", s.qrImage)
	mux.HandleFunc("GET /events/{id}", s.attendeePage)
	// Event links are /{slug}; /events/{id} stays for QR codes printed before slugs.
	mux.HandleFunc("GET /{id}", s.eventPage)
	mux.HandleFunc("GET /{$}", s.homePage)
	mux.Handle("GET /static/", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := &timings{entries: map[string]*timing{}}
		r = r.WithContext(context.WithValue(r.Context(), timingsKey{}, t))
		writer := &responseWriter{ResponseWriter: w, timings: t, start: time.Now(), acceptsGzip: acceptsGzip(r)}
		defer writer.finish()
		w = writer
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

// writes turns a change away while the storage cut-over has writes paused.
func (s *server) writes(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.config.WritesPaused {
			handleError(w, errPaused)
			return
		}
		next(w, r)
	}
}

// loginURL is the sign-in link that returns to path: the platform's prefix
// followed by the URL-encoded return URL, or else the catalog's login.
func (s *server) loginURL(path string) string {
	if s.config.PlatformLoginURL != "" {
		return s.config.PlatformLoginURL + url.QueryEscape(s.config.PublicURL+path)
	}
	return s.config.AuthBaseURL + "/auth/login?" + url.Values{"return_to": {s.config.PublicURL + path}}.Encode()
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.gateway.Health(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "Temporal is unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) session(w http.ResponseWriter, r *http.Request) {
	employee, err := s.employee(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errAuthUnavailable.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Authenticated bool   `json:"authenticated"`
		Email         string `json:"email,omitempty"`
		LoginURL      string `json:"loginUrl"`
	}{employee.Email != "", employee.Email, s.loginURL("/admin")})
}

// linkPreview fills the page's Open Graph tags, which Slack and other apps read
// when a link is shared. Those crawlers don't run JavaScript, and need an
// absolute image URL.
type linkPreview struct {
	Title, Description, URL, ImageURL string
}

// renderIndex serves the app's page. bootstrap holds the responses of the API
// calls the page makes first, keyed by path; the app uses each once instead of
// fetching it, which saves the browser those round trips.
func (s *server) renderIndex(w http.ResponseWriter, preview *linkPreview, bootstrap map[string]json.RawMessage) {
	if bootstrap == nil {
		bootstrap = map[string]json.RawMessage{}
	}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		data = []byte("{}")
	}
	var body bytes.Buffer
	if err := s.indexPage.Execute(&body, struct {
		Preview   *linkPreview
		Bootstrap string
	}{preview, string(data)}); err != nil {
		writeError(w, http.StatusInternalServerError, "Page is unavailable.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

// prefetch runs the page's first API reads within this request, as the browser
// would, with its cookies. Only successful responses are kept; for anything
// else the app makes the request itself and handles the error as usual.
func (s *server) prefetch(r *http.Request, paths ...string) map[string]json.RawMessage {
	results := make([]json.RawMessage, len(paths))
	var wait sync.WaitGroup
	for i, path := range paths {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := httptest.NewRequestWithContext(r.Context(), http.MethodGet, path, nil)
			for _, cookie := range r.Cookies() {
				request.AddCookie(cookie)
			}
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, request)
			if response.Code == http.StatusOK {
				results[i] = response.Body.Bytes()
			}
		}()
	}
	wait.Wait()
	bootstrap := map[string]json.RawMessage{}
	for i, path := range paths {
		if results[i] != nil {
			bootstrap[path] = results[i]
		}
	}
	return bootstrap
}

func (s *server) homePage(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, nil, s.prefetch(r, "/api/session"))
}

func (s *server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, nil, s.prefetch(r, "/api/session", "/api/admin/events"))
}

func (s *server) eventDetailPage(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		s.renderIndex(w, nil, s.prefetch(r, "/api/session"))
		return
	}
	path := "/api/admin/events/" + url.PathEscape(id)
	bootstrap := s.prefetch(r, "/api/session", path)
	// The app reads participants by the event's own ID, which differs when it
	// was opened through an earlier link.
	var event struct{ ID string }
	if json.Unmarshal(bootstrap[path], &event) == nil && event.ID != "" {
		maps.Copy(bootstrap, s.prefetch(r, "/api/admin/events/"+url.PathEscape(event.ID)+"/participants"))
	}
	s.renderIndex(w, nil, bootstrap)
}

func (s *server) eventPage(w http.ResponseWriter, r *http.Request) {
	if _, err := eventID(r); err != nil {
		http.NotFound(w, r)
		return
	}
	s.attendeePage(w, r)
}

// attendeePage serves the app with the event's link preview. An event that
// can't be loaded gets the generic page, which shows the error itself.
func (s *server) attendeePage(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		s.renderIndex(w, nil, nil)
		return
	}
	preview := event.EffectivePreview()
	// The app reads the event by the path it was opened with.
	bootstrap := s.prefetch(r, "/api/events/"+url.PathEscape(r.PathValue("id")))
	// Slack caches unfurl images by URL, so the versioned URL shows a changed image.
	s.renderIndex(w, &linkPreview{
		Title: preview.Title, Description: preview.Description,
		URL: s.config.PublicURL + "/" + event.Link(), ImageURL: s.config.PublicURL + s.static.URL("link-preview.png"),
	}, bootstrap)
}

func eventID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if id == "" || len(id) > 80 {
		return "", errors.New("invalid event ID")
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return "", errors.New("invalid event ID")
		}
	}
	return id, nil
}

func (s *server) getEvent(r *http.Request) (tracker.Event, error) {
	id, err := eventID(r)
	if err != nil {
		return tracker.Event{}, &inputError{err.Error()}
	}
	return s.visibleEvent(r.Context(), id)
}

func (s *server) visibleEvent(ctx context.Context, id string) (tracker.Event, error) {
	event, err := s.gateway.Event(ctx, id)
	if err != nil || event.DeletedAt != nil {
		if err == nil {
			err = errDeleted
		}
		return tracker.Event{}, err
	}
	return event, nil
}

func (s *server) publicEvent(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	// RespondedAt is set when the employee has already responded; the event's
	// workflow rejects a second response, so the page shows that up front.
	type participant struct {
		Name        string     `json:"name,omitempty"`
		Email       string     `json:"email"`
		RespondedAt *time.Time `json:"respondedAt,omitempty"`
	}
	var signedIn *participant
	if event.InternalOnly {
		employee, ok := s.internalParticipant(w, r, event)
		if !ok {
			return
		}
		lead, err := s.gateway.Participant(r.Context(), event.ID, employee.Email)
		if err != nil {
			handleError(w, err)
			return
		}
		signedIn = &participant{Name: employee.Name, Email: employee.Email}
		if lead != nil {
			signedIn.RespondedAt = &lead.LastSubmittedAt
		}
	}
	writeJSON(w, http.StatusOK, struct {
		ID           string       `json:"id"`
		Name         string       `json:"name"`
		Subheading   string       `json:"subheading"`
		Description  string       `json:"description"`
		EndDate      string       `json:"endDate"`
		ClosesAt     time.Time    `json:"closesAt"`
		Status       string       `json:"status"`
		Form         tracker.Form `json:"form"`
		InternalOnly bool         `json:"internalOnly,omitempty"`
		Participant  *participant `json:"participant,omitempty"`
	}{event.ID, event.Name, event.EffectiveSubheading(), event.Description, event.EndDate, event.ClosesAt, event.Status, event.EffectiveForm(), event.InternalOnly, signedIn})
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var input tracker.Submission
	if !s.decode(w, r, &input, false) {
		return
	}
	event, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if event.InternalOnly {
		// A submission that carries the employee's cookie is held to the same
		// origin rule as admin mutations.
		if r.Header.Get("Origin") == "" {
			writeError(w, http.StatusForbidden, "Submit this request from the event tracker page.")
			return
		}
		employee, ok := s.internalParticipant(w, r, event)
		if !ok {
			return
		}
		input = input.WithParticipant(event, employee.Name, employee.Email)
	}
	input, err = input.WithoutStaleAnswers(event).NormalizeForEvent(event)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An ended, still-running event can acknowledge a previously saved request.
	if event.Status == "archived" {
		writeError(w, http.StatusGone, "This event is no longer accepting submissions.")
		return
	}
	if err := s.gateway.Submit(r.Context(), event.ID, input); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (s *server) listEvents(w http.ResponseWriter, r *http.Request) {
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 8192 {
		handleError(w, errInvalidCursor)
		return
	}
	events, next, err := s.gateway.List(r.Context(), cursor)
	if err != nil {
		handleError(w, err)
		return
	}
	visible := make([]tracker.Event, 0, len(events))
	for _, event := range events {
		if event.DeletedAt == nil {
			visible = append(visible, event)
		}
	}
	events = visible
	writeJSON(w, http.StatusOK, struct {
		Events     []tracker.Event `json:"events"`
		NextCursor string          `json:"nextCursor"`
	}{events, next})
}

func (s *server) createEvent(w http.ResponseWriter, r *http.Request) {
	var input tracker.CreateEvent
	if !s.decode(w, r, &input, true) {
		return
	}
	email, _ := r.Context().Value(employeeContextKey{}).(string)
	event, err := tracker.NewEvent("pending", email, input, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The store reserves the slug: a taken one, or one whose workflow is still
	// running, rejects the create, so try the next suffix.
	event, err = tracker.CreateWithSlug(r.Context(), event, s.gateway.Create, nil)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Location", "/api/admin/events/"+event.ID)
	writeJSON(w, http.StatusCreated, event)
}

func (s *server) adminEvent(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) endEvent(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct{}
	if !s.decode(w, r, &body, true) {
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status != "open" {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	event, err := s.gateway.End(r.Context(), existing.ID)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct{}
	if !s.decode(w, r, &body, true) {
		return
	}
	if err := s.gateway.Delete(r.Context(), id); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *server) banner(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Banner string `json:"banner"`
	}
	if !s.decode(w, r, &body, true) {
		return
	}
	body.Banner = strings.TrimSpace(body.Banner)
	if utf8.RuneCountInString(body.Banner) > 500 {
		writeError(w, http.StatusBadRequest, "QR banner must be 500 characters or fewer.")
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its QR banner cannot be changed.")
		return
	}
	event, err := s.gateway.Banner(r.Context(), existing.ID, body.Banner)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body tracker.LinkPreview
	if !s.decode(w, r, &body, true) {
		return
	}
	preview, err := body.Normalize()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its link preview cannot be changed.")
		return
	}
	event, err := s.gateway.Preview(r.Context(), existing.ID, preview)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) form(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// InternalOnly makes a public event internal-only along with the new form;
	// false leaves the event as it is, so an internal-only event stays that way.
	var body struct {
		Form         tracker.Form `json:"form"`
		InternalOnly bool         `json:"internalOnly"`
	}
	if !s.decode(w, r, &body, true) {
		return
	}
	form, err := body.Form.Normalize()
	if err == nil && body.InternalOnly {
		err = form.RequireIdentityFields()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its questions cannot be changed.")
		return
	}
	var event tracker.Event
	if body.InternalOnly && !existing.InternalOnly {
		event, err = s.gateway.InternalOnly(r.Context(), existing.ID, form)
	} else {
		event, err = s.gateway.Form(r.Context(), existing.ID, form)
	}
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) details(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body tracker.EventDetails
	if !s.decode(w, r, &body, true) {
		return
	}
	details, _, err := body.Normalize()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its details cannot be changed.")
		return
	}
	event, err := s.gateway.Details(r.Context(), existing.ID, details)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) slug(w http.ResponseWriter, r *http.Request) {
	id, err := eventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body struct {
		Slug string `json:"slug"`
	}
	if !s.decode(w, r, &body, true) {
		return
	}
	slug, err := tracker.NormalizeSlug(body.Slug)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.visibleEvent(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its link cannot be changed.")
		return
	}
	event, err := s.gateway.Slug(r.Context(), existing.ID, slug)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *server) decode(w http.ResponseWriter, r *http.Request, target any, requireOrigin bool) bool {
	origin := r.Header.Get("Origin")
	if (requireOrigin && origin == "") || (origin != "" && origin != s.config.PublicURL) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "Submit this request from the event tracker page.")
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Use application/json.")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON request.")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "Invalid JSON request.")
		return false
	}
	return true
}

const qrHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}}</title><link rel="icon" href="/static/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="/static/qr.css"></head>
<body{{if .AllowMotion}} class="allow-motion"{{end}}><main class="qr-display"><div class="qr-copy"><img class="brand" src="/static/temporal-logo.svg" alt="Temporal"><h1>{{.Name}}</h1><p class="banner">{{.Banner}}</p></div><div class="qr-panel"><div class="qr-stage"><div class="ziggy" aria-hidden="true"><img class="ziggy-open" src="/static/ziggy-peek.png" alt=""><img class="ziggy-blink" src="/static/ziggy-blink.png" alt=""></div><div class="qr-frame"><img class="qr-code" src="{{.ImageURL}}" alt="Scan to open the event form"></div></div></div></main></body></html>`

func (s *server) qrPage(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	banner := event.QRBanner
	if strings.TrimSpace(banner) == "" {
		banner = "Scan to stay in touch."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.qrDisplay.Execute(w, struct {
		Name, Banner, ImageURL string
		AllowMotion            bool
	}{event.Name, banner, "/admin/events/" + event.ID + "/qr.png", r.URL.Query().Get("motion") == "on"})
}

func (s *server) qrImage(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	data, err := qrcode.EncodePNG(s.config.PublicURL+"/"+event.Link(), 1024)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
}

type inputError struct{ message string }

func (e *inputError) Error() string { return e.message }

func handleError(w http.ResponseWriter, err error) {
	var invalid *inputError
	var application *temporal.ApplicationError
	var missing *serviceerror.NotFound
	status, message := http.StatusServiceUnavailable, "Temporal is temporarily unavailable. Please retry."
	switch {
	case errors.As(err, &invalid):
		status, message = http.StatusBadRequest, invalid.message
	case errors.Is(err, errInvalidCursor):
		status, message = http.StatusBadRequest, errInvalidCursor.Error()
	case errors.Is(err, errChanged):
		status, message = http.StatusConflict, errChanged.Error()
	case errors.Is(err, errPaused):
		message = errPaused.Error()
	case errors.Is(err, errDeleted):
		status, message = http.StatusNotFound, "This event was deleted."
	case errors.Is(err, tracker.ErrNotStored):
		status, message = http.StatusNotFound, "Event not found."
	case errors.As(err, &missing):
		// The event is stored but its workflow isn't running yet, such as just
		// after a namespace reset; the reconciler starts it.
		message = "Event is starting. Please retry shortly."
	case errors.As(err, &application):
		switch application.Type() {
		case "Invalid":
			status, message = http.StatusBadRequest, application.Message()
		case "Closed":
			status, message = http.StatusGone, application.Message()
		case "AlreadyResponded":
			status, message = http.StatusConflict, "You've already responded to this event."
		case "Refreshing":
			message = "Event is refreshing. Please retry shortly."
		case "Paused":
			message = errPaused.Error()
		}
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "2")
	}
	writeError(w, status, message)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		http.Error(w, "Response encoding failed.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body.String())
}
