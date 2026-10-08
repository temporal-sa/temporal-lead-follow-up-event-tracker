package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	PublicURL     string
	AuthBaseURL   string
	AuthVerifyURL string
	DevAuthEmail  string
}

type server struct {
	config       Config
	gateway      Gateway
	publicOrigin *url.URL
	authClient   *http.Client
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
	s := &server{
		config: config, gateway: gateway, publicOrigin: public,
		authClient: &http.Client{
			Transport: authTransport,
			Timeout:   authTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/events/{id}", s.publicEvent)
	mux.HandleFunc("POST /api/events/{id}/participants", s.submit)
	mux.HandleFunc("GET /api/admin/events", s.admin(s.listEvents))
	mux.HandleFunc("POST /api/admin/events", s.admin(s.createEvent))
	mux.HandleFunc("GET /api/admin/events/{id}", s.admin(s.adminEvent))
	mux.HandleFunc("POST /api/admin/events/{id}/end", s.admin(s.endEvent))
	mux.HandleFunc("POST /api/admin/events/{id}/banner", s.admin(s.banner))
	mux.HandleFunc("GET /api/admin/events/{id}/participants", s.admin(s.participants))
	mux.HandleFunc("GET /api/admin/events/{id}/export.csv", s.admin(s.export))
	mux.HandleFunc("GET /admin/events/{id}/qr", s.admin(s.qrPage))
	mux.HandleFunc("GET /admin/events/{id}/qr.png", s.admin(s.qrImage))
	mux.HandleFunc("GET /admin", s.admin(s.index))
	mux.HandleFunc("GET /admin/events/{id}", s.admin(s.index))
	mux.HandleFunc("GET /events/{id}", s.index)
	mux.HandleFunc("GET /{$}", s.index)
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func (s *server) loginURL() string {
	return s.config.AuthBaseURL + "/auth/login?" + url.Values{"return_to": {s.config.PublicURL + "/admin"}}.Encode()
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
	email, err := s.employee(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errAuthUnavailable.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Authenticated bool   `json:"authenticated"`
		Email         string `json:"email,omitempty"`
		LoginURL      string `json:"loginUrl"`
	}{email != "", email, s.loginURL()})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("static/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Page is unavailable.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
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
	return s.gateway.Event(r.Context(), id)
}

func (s *server) publicEvent(w http.ResponseWriter, r *http.Request) {
	event, err := s.getEvent(r)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID          string       `json:"id"`
		Name        string       `json:"name"`
		Description string       `json:"description"`
		EndDate     string       `json:"endDate"`
		ClosesAt    time.Time    `json:"closesAt"`
		Status      string       `json:"status"`
		Form        tracker.Form `json:"form"`
	}{event.ID, event.Name, event.Description, event.EndDate, event.ClosesAt, event.Status, event.EffectiveForm()})
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
	event, err := s.gateway.Event(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	input, err = input.NormalizeForEvent(event)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An ended, still-running event can acknowledge a previously saved request.
	if event.Status == "archived" {
		writeError(w, http.StatusGone, "This event is no longer accepting submissions.")
		return
	}
	if err := s.gateway.Submit(r.Context(), id, input); err != nil {
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
	if events == nil {
		events = []tracker.Event{}
	}
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
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		handleError(w, err)
		return
	}
	email, _ := r.Context().Value(employeeContextKey{}).(string)
	event, err := tracker.NewEvent(hex.EncodeToString(idBytes[:]), email, input, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.gateway.Create(r.Context(), event); err != nil {
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
	existing, err := s.gateway.Event(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status != "open" {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	event, err := s.gateway.End(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
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
	existing, err := s.gateway.Event(r.Context(), id)
	if err != nil {
		handleError(w, err)
		return
	}
	if existing.Status == "archived" {
		writeError(w, http.StatusGone, "This event has completed; its QR banner cannot be changed.")
		return
	}
	event, err := s.gateway.Banner(r.Context(), id, body.Banner)
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

var qrTemplate = template.Must(template.New("qr").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Name}} · Temporal</title><link rel="stylesheet" href="/static/qr.css"></head>
<body{{if .AllowMotion}} class="allow-motion"{{end}}><main class="qr-display"><div class="qr-copy"><img class="brand" src="/static/temporal-logo.svg" alt="Temporal"><p class="eyebrow">LET’S KEEP THE CONVERSATION GOING</p><h1>{{.Name}}</h1><p class="banner">{{.Banner}}</p><p class="qr-instruction">Open your camera. Scan the code.<span>Tell us what you’d like to explore.</span></p></div><div class="qr-panel"><div class="qr-stage"><div class="ziggy" aria-hidden="true"><img class="ziggy-open" src="/static/ziggy-peek.png" alt=""><img class="ziggy-blink" src="/static/ziggy-blink.png" alt=""></div><div class="qr-frame"><img class="qr-code" src="{{.ImageURL}}" alt="Scan to open the event follow-up form"></div></div><p class="qr-caption">YOUR NEXT CONVERSATION STARTS HERE <span aria-hidden="true">↗</span></p></div></main></body></html>`))

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
	_ = qrTemplate.Execute(w, struct {
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
	data, err := qrcode.EncodePNG(s.config.PublicURL+"/events/"+event.ID, 1024)
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
	case errors.Is(err, errExpired):
		status, message = http.StatusGone, errExpired.Error()
	case errors.As(err, &missing):
		status, message = http.StatusNotFound, "Event not found or no longer retained."
	case errors.As(err, &application):
		switch application.Type() {
		case "Invalid":
			status, message = http.StatusBadRequest, application.Message()
		case "Closed":
			status, message = http.StatusGone, application.Message()
		case "Refreshing":
			message = "Event is refreshing. Please retry shortly."
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
