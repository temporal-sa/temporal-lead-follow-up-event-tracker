package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

const (
	validSession = "opaque-catalog-session"
	verifierURL  = "http://web.tmprl-dem-cld-registry-operator.svc.cluster.local/_auth/verify"
)

type verifierTransport func(*http.Request) (*http.Response, error)

func (v verifierTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return v(r)
}

func verifierResponse(status int, subject, email string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"X-Temporal-Auth-Subject": {subject},
			"X-Temporal-Auth-Email":   {email},
		},
		Body: io.NopCloser(strings.NewReader("")),
	}
}

func testHandlerWithVerifier(t *testing.T, g Gateway, transport http.RoundTripper) http.Handler {
	t.Helper()
	h, err := newHandler(Config{PublicURL: "https://leads.tmprl-demo.cloud", AuthVerifyURL: verifierURL}, g, transport)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEmployeeAuthenticationViaCatalog(t *testing.T) {
	tests := []struct {
		name, subject, email string
		status, want         int
	}{
		{"valid", "google:employee-id", "employee@temporal.io", 204, 200},
		{"uppercase domain", "google:employee-id", "Employee@TEMPORAL.IO", 204, 200},
		{"wrong domain", "google:visitor-id", "visitor@example.com", 204, 401},
		{"domain suffix", "google:visitor-id", "visitor@temporal.io.example", 204, 401},
		{"display name", "google:employee-id", "Employee <employee@temporal.io>", 204, 401},
		{"bootstrap identity", "bootstrap-catalog-auth", "bootstrap@temporal.io", 204, 401},
		{"missing subject", "", "employee@temporal.io", 204, 503},
		{"missing email", "google:employee-id", "", 204, 503},
		{"login redirect", "google:employee-id", "employee@temporal.io", 302, 401},
		{"unauthorized", "google:employee-id", "employee@temporal.io", 401, 401},
		{"forbidden", "google:employee-id", "employee@temporal.io", 403, 401},
		{"upstream failure", "google:employee-id", "employee@temporal.io", 500, 503},
		{"unexpected success", "google:employee-id", "employee@temporal.io", 200, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := testHandlerWithVerifier(t, &fakeGateway{}, verifierTransport(func(*http.Request) (*http.Response, error) {
				return verifierResponse(tt.status, tt.subject, tt.email), nil
			}))
			w := request(t, h, "GET", "/api/admin/events", "", validSession, "")
			if w.Code != tt.want {
				t.Fatalf("admin status = %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			w = request(t, h, "GET", "/api/session", "", validSession, "")
			if tt.want == 503 {
				if w.Code != 503 || !strings.Contains(w.Body.String(), "Employee authentication is unavailable") {
					t.Fatalf("session failure = %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != 200 || strings.Contains(w.Body.String(), `"authenticated":true`) != (tt.want == 200) {
				t.Fatalf("session = %d %s", w.Code, w.Body.String())
			}
			if tt.want == 200 && !strings.Contains(w.Body.String(), `"email":"employee@temporal.io"`) {
				t.Fatalf("session email not normalized: %s", w.Body.String())
			}
		})
	}
}

func TestVerifierGetsOnlySessionAndTrustedRequestMetadata(t *testing.T) {
	calls := 0
	g := &fakeGateway{}
	h := testHandlerWithVerifier(t, g, verifierTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != verifierURL || r.Method != http.MethodGet || r.Body != nil {
			t.Errorf("verifier request = %s %s, body=%v", r.Method, r.URL, r.Body)
		}
		cookies := r.Cookies()
		if len(cookies) != 1 || cookies[0].Name != "temporal_demo_auth" || cookies[0].Value != validSession {
			t.Errorf("unexpected verifier cookies: %v", cookies)
		}
		wantHeaders := map[string]string{
			"X-Forwarded-Host":   "leads.tmprl-demo.cloud",
			"X-Forwarded-Proto":  "https",
			"X-Forwarded-Method": "POST",
			"X-Forwarded-Uri":    "/api/admin/events?source=demo",
		}
		for key, want := range wantHeaders {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		for _, key := range []string{"Authorization", "Origin", "X-Temporal-Auth-Email", "X-Temporal-Auth-Subject", "X-Temporal-Auth-JWT"} {
			if r.Header.Get(key) != "" {
				t.Errorf("forwarded caller header %s", key)
			}
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > authTimeout {
			t.Error("verification lacks a bounded timeout")
		}
		return verifierResponse(204, "google:verified-id", "Verified@TEMPORAL.IO"), nil
	}))
	r := httptest.NewRequest("POST", "/api/admin/events?source=demo", strings.NewReader(`{"name":"Conference","endDate":"2099-01-01"}`))
	r.Host = "attacker.example"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://leads.tmprl-demo.cloud")
	r.Header.Set("Authorization", "Bearer caller-token")
	r.AddCookie(&http.Cookie{Name: "temporal_demo_auth", Value: validSession})
	r.AddCookie(&http.Cookie{Name: "unrelated", Value: "private"})
	for _, key := range []string{"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Method", "X-Forwarded-Uri", "X-Temporal-Auth-Email", "X-Temporal-Auth-Subject", "X-Temporal-Auth-JWT"} {
		r.Header.Set(key, "attacker-supplied")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 || calls != 1 || g.created == nil || g.created.CreatedBy != "verified@temporal.io" {
		t.Fatalf("create = %d, verification calls=%d, event=%#v", w.Code, calls, g.created)
	}
}

func TestVerifierRedirectsAreNotFollowed(t *testing.T) {
	calls := 0
	h := testHandlerWithVerifier(t, &fakeGateway{}, verifierTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			return verifierResponse(204, "google:employee-id", "employee@temporal.io"), nil
		}
		response := verifierResponse(302, "", "")
		response.Header.Set("Location", "https://other.example/login")
		return response, nil
	}))
	w := request(t, h, "GET", "/api/admin/events", "", validSession, "")
	if w.Code != 401 || calls != 1 {
		t.Fatalf("redirect followed: status=%d calls=%d", w.Code, calls)
	}
}

func TestVerifierUnavailableBlocksEveryAdminRoute(t *testing.T) {
	routes := []struct{ method, path string }{
		{"GET", "/api/admin/events"},
		{"POST", "/api/admin/events"},
		{"GET", "/api/admin/events/demo"},
		{"POST", "/api/admin/events/demo/end"},
		{"POST", "/api/admin/events/demo/banner"},
		{"GET", "/api/admin/events/demo/participants"},
		{"GET", "/api/admin/events/demo/export.csv"},
		{"GET", "/admin"},
		{"GET", "/admin/events/demo"},
		{"GET", "/admin/events/demo/qr"},
		{"GET", "/admin/events/demo/qr.png"},
		{"GET", "/api/session"},
	}
	for _, failure := range []error{context.DeadlineExceeded, errors.New("catalog connection failed")} {
		h := testHandlerWithVerifier(t, &fakeGateway{}, verifierTransport(func(*http.Request) (*http.Response, error) {
			return nil, failure
		}))
		for _, route := range routes {
			w := request(t, h, route.method, route.path, "", validSession, "")
			if w.Code != 503 || !strings.Contains(w.Body.String(), "Employee authentication is unavailable") {
				t.Errorf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
		}
	}
}

func TestPublicFormsAndMissingSessionsDoNotCallVerifier(t *testing.T) {
	calls := 0
	h := testHandlerWithVerifier(t, &fakeGateway{event: tracker.Event{ID: "demo", Status: "open"}}, verifierTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("catalog unavailable")
	}))
	for _, path := range []string{"/", "/events/demo", "/api/events/demo", "/healthz"} {
		w := request(t, h, "GET", path, "", validSession, "")
		if w.Code != 200 {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	body := `{"name":"Ada","role":"Engineer","email":"ada@example.com","requestId":"01234567-89ab-cdef-0123-456789abcdef"}`
	if w := request(t, h, "POST", "/api/events/demo/participants", body, validSession, ""); w.Code != 200 {
		t.Fatalf("public submission failed: %d %s", w.Code, w.Body.String())
	}
	for _, token := range []string{"", strings.Repeat("a", 8193)} {
		r := httptest.NewRequest("GET", "/api/admin/events", nil)
		if token != "" {
			r.AddCookie(&http.Cookie{Name: "temporal_demo_auth", Value: token})
		}
		r.Header.Set("X-Temporal-Auth-Email", "employee@temporal.io")
		r.Header.Set("X-Temporal-Auth-Subject", "google:employee-id")
		r.Header.Set("X-Temporal-Auth-JWT", validSession)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("identity headers or invalid session authenticated: %d", w.Code)
		}
	}
	w := request(t, h, "GET", "/api/session", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":false`) || calls != 0 {
		t.Fatalf("anonymous session = %d %s, verifier calls=%d", w.Code, w.Body.String(), calls)
	}
}

func TestProtectedRequestsAreVerifiedEveryTime(t *testing.T) {
	calls := 0
	h := testHandlerWithVerifier(t, &fakeGateway{}, verifierTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return verifierResponse(204, "google:employee-id", "employee@temporal.io"), nil
		}
		return verifierResponse(401, "", ""), nil
	}))
	first := request(t, h, "GET", "/api/admin/events", "", validSession, "")
	second := request(t, h, "GET", "/api/admin/events", "", validSession, "")
	if first.Code != 200 || second.Code != 401 || calls != 2 {
		t.Fatalf("cached authentication: first=%d second=%d calls=%d", first.Code, second.Code, calls)
	}
}

func TestAuthenticationURLs(t *testing.T) {
	for _, verifyURL := range []string{"file:///tmp/auth", "/_auth/verify", "https://user:pass@catalog.example/_auth/verify", "https://catalog.example/_auth/verify?token=secret", "https://catalog.example/_auth/verify#fragment"} {
		if _, err := New(Config{PublicURL: "https://leads.tmprl-demo.cloud", AuthVerifyURL: verifyURL}, &fakeGateway{}); err == nil {
			t.Errorf("invalid AUTH_VERIFY_URL accepted: %s", verifyURL)
		}
	}
	if _, err := New(Config{PublicURL: "https://leads.tmprl-demo.cloud", AuthBaseURL: "javascript:login"}, &fakeGateway{}); err == nil {
		t.Fatal("invalid login origin accepted")
	}
	if _, err := New(Config{PublicURL: "https://leads.tmprl-demo.cloud"}, &fakeGateway{}); err != nil {
		t.Fatalf("default verifier should not require a signing key: %v", err)
	}
}
