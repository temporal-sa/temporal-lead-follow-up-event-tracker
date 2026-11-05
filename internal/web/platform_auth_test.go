package web

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	platformVerifyURL = "http://oauth2-proxy.fieldkit-auth.svc.cluster.local/oauth2/auth"
	platformLoginURL  = "https://auth.test.tmprl-demo.cloud/oauth2/start?rd="
	platformCookie    = "temporal_demo_auth_test"
)

func platformConfig(trust bool) Config {
	return Config{
		PublicURL:         "https://events.test.tmprl-demo.cloud",
		PlatformVerifyURL: platformVerifyURL, PlatformCookie: platformCookie, PlatformLoginURL: platformLoginURL,
		TrustIdentityHeaders: trust,
	}
}

func platformHandler(t *testing.T, config Config, g Gateway, transport http.RoundTripper) http.Handler {
	t.Helper()
	h, err := newHandler(config, g, transport)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func platformResponse(status int, email, name string) *http.Response {
	h := http.Header{}
	if email != "" {
		h.Set("X-Temporal-Auth-Subject", "1234567890")
		h.Set("X-Temporal-Auth-Email", email)
	}
	if name != "" {
		h.Set("X-Temporal-Auth-Name", name)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(""))}
}

func noVerifier(t *testing.T) verifierTransport {
	return func(*http.Request) (*http.Response, error) {
		t.Error("the verifier was called")
		return nil, errors.New("unexpected")
	}
}

// Behind the platform's edge, the identity headers are the employee; no
// verifier call is needed.
func TestTrustedIdentityHeadersSignInWithoutAVerifierCall(t *testing.T) {
	g := &fakeGateway{}
	h := platformHandler(t, platformConfig(true), g, noVerifier(t))
	r := httptest.NewRequest("POST", "/api/admin/events", strings.NewReader(`{"name":"Conference","endDate":"2099-01-01"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://events.test.tmprl-demo.cloud")
	r.Header.Set("X-Temporal-Auth-Email", "Edge@Temporal.io")
	r.Header.Set("X-Temporal-Auth-Name", "Edge Employee")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 || g.created == nil || g.created.CreatedBy != "edge@temporal.io" {
		t.Fatalf("create = %d %s, event=%#v", w.Code, w.Body.String(), g.created)
	}
	for _, email := range []string{"visitor@example.com", "Edge Employee <edge@temporal.io>"} {
		r := httptest.NewRequest("GET", "/api/admin/events", nil)
		r.Header.Set("X-Temporal-Auth-Email", email)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%s: %d", email, w.Code)
		}
	}
}

// Untrusted identity headers never sign anyone in, in either mode.
func TestUntrustedIdentityHeadersAreIgnored(t *testing.T) {
	for _, config := range []Config{platformConfig(false), {PublicURL: "https://leads.tmprl-demo.cloud", AuthVerifyURL: verifierURL}} {
		h := platformHandler(t, config, &fakeGateway{}, verifierTransport(func(*http.Request) (*http.Response, error) {
			return platformResponse(401, "", ""), nil
		}))
		r := httptest.NewRequest("GET", "/api/admin/events", nil)
		r.Header.Set("X-Temporal-Auth-Email", "employee@temporal.io")
		r.Header.Set("X-Temporal-Auth-Subject", "forged")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Errorf("%+v: forged headers gave %d", config, w.Code)
		}
	}
}

// Without identity headers, the platform's verifier gets the whole Cookie
// header and nothing else; any 2xx with an email is signed in.
func TestPlatformVerifierFallback(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		email        string
		admin, after int
	}{
		{"accepted (oauth2-proxy)", 202, "employee@temporal.io", 200, 200},
		{"ok", 200, "Employee@TEMPORAL.IO", 200, 200},
		{"no content", 204, "employee@temporal.io", 200, 200},
		{"2xx without identity", 202, "", 401, 401},
		{"wrong domain", 202, "visitor@example.com", 401, 401},
		{"unauthorized", 401, "", 401, 401},
		{"forbidden", 403, "", 401, 401},
		{"redirect", 302, "", 401, 401},
		{"upstream failure", 500, "", 503, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			h := platformHandler(t, platformConfig(true), &fakeGateway{}, verifierTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != platformVerifyURL || r.Method != http.MethodGet {
					t.Errorf("verifier request = %s %s", r.Method, r.URL)
				}
				if got := r.Header.Get("Cookie"); got != platformCookie+"_0=part0; "+platformCookie+"_1=part1; other=x" {
					t.Errorf("Cookie = %q", got)
				}
				for key := range r.Header {
					if key != "Cookie" {
						t.Errorf("forwarded header %s", key)
					}
				}
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > authTimeout {
					t.Error("verification lacks a bounded timeout")
				}
				return platformResponse(tt.status, tt.email, "Zoë"), nil
			}))
			do := func(path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Cookie", platformCookie+"_0=part0; "+platformCookie+"_1=part1")
				r.Header.Add("Cookie", "other=x")
				r.Header.Set("Authorization", "Bearer caller-token")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			if w := do("/api/admin/events"); w.Code != tt.admin {
				t.Fatalf("admin = %d, want %d: %s", w.Code, tt.admin, w.Body.String())
			}
			w := do("/api/session")
			switch tt.after {
			case 503:
				if w.Code != 503 {
					t.Fatalf("session = %d", w.Code)
				}
			default:
				if w.Code != 200 || strings.Contains(w.Body.String(), `"authenticated":true`) != (tt.after == 200) {
					t.Fatalf("session = %d %s", w.Code, w.Body.String())
				}
				if calls != 1 {
					t.Errorf("verifier calls = %d, want 1 (cached per Cookie header)", calls)
				}
			}
		})
	}
}

func TestPlatformWithoutCookiesIsAnonymousWithoutACall(t *testing.T) {
	h := platformHandler(t, platformConfig(true), &fakeGateway{}, noVerifier(t))
	w := request(t, h, "GET", "/api/session", "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatalf("session = %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/admin/events", nil)
	r.Header.Set("Cookie", platformCookie+"="+strings.Repeat("a", maxCookieHeader))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("oversized Cookie header: %d", w.Code)
	}
}

// Sign-in links are FIELDKIT_LOGIN_URL plus the URL-encoded return URL.
func TestPlatformLoginURL(t *testing.T) {
	h := platformHandler(t, platformConfig(true), &fakeGateway{}, noVerifier(t))
	w := request(t, h, "GET", "/admin/events/demo/qr", "", "", "")
	want := platformLoginURL + url.QueryEscape("https://events.test.tmprl-demo.cloud/admin")
	if w.Code != 302 || w.Header().Get("Location") != want {
		t.Fatalf("redirect = %d %s, want %s", w.Code, w.Header().Get("Location"), want)
	}
	location, _ := url.Parse(w.Header().Get("Location"))
	if location.Query().Get("rd") != "https://events.test.tmprl-demo.cloud/admin" {
		t.Fatalf("rd = %q", location.Query().Get("rd"))
	}
	w = request(t, h, "GET", "/api/session", "", "", "")
	if !strings.Contains(w.Body.String(), `"loginUrl":"`+strings.ReplaceAll(want, "&", `&`)+`"`) {
		t.Fatalf("session = %s", w.Body.String())
	}
}

// The platform's login link applies even with the catalog's verifier, and
// the catalog's login stays without it.
func TestLoginURLWithoutPlatformVerifier(t *testing.T) {
	h := platformHandler(t, Config{PublicURL: "https://leads.tmprl-demo.cloud", PlatformLoginURL: platformLoginURL}, &fakeGateway{}, noVerifier(t))
	w := request(t, h, "GET", "/admin", "", "", "")
	if got := w.Header().Get("Location"); !strings.HasPrefix(got, platformLoginURL) {
		t.Fatalf("login = %s", got)
	}
}

func TestPlatformURLsAreValidated(t *testing.T) {
	for _, bad := range []string{"/oauth2/auth", "file:///x", "http://u:p@proxy/oauth2/auth", "http://proxy/oauth2/auth?x=1", "http://proxy/oauth2/auth#f"} {
		c := platformConfig(true)
		c.PlatformVerifyURL = bad
		if _, err := New(c, &fakeGateway{}); err == nil {
			t.Errorf("FIELDKIT_AUTH_VERIFY_URL %q accepted", bad)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "/oauth2/start?rd=", "https://u:p@auth.example/start?rd=", "https://auth.example/start#rd="} {
		c := platformConfig(true)
		c.PlatformLoginURL = bad
		if _, err := New(c, &fakeGateway{}); err == nil {
			t.Errorf("FIELDKIT_LOGIN_URL %q accepted", bad)
		}
	}
}
