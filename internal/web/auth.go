package web

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

const authTimeout = 3 * time.Second

var errAuthUnavailable = errors.New("Employee authentication is unavailable. Please try again.")

type employeeContextKey struct{}

// identity is a verified employee. Name is empty when the catalog has none.
type identity struct {
	Email string
	Name  string
}

// An empty email with no error means the visitor is not authenticated.
func (s *server) employee(r *http.Request) (identity, error) {
	if s.config.DevAuthEmail != "" {
		return identity{Email: strings.ToLower(s.config.DevAuthEmail)}, nil
	}
	cookie, err := r.Cookie("temporal_demo_auth")
	if err != nil || cookie.Value == "" || len(cookie.Value) > 8192 {
		return identity{}, nil
	}
	if cached, ok := s.sessions.get(cookie.Value); ok {
		return cached, nil
	}
	verified, err := s.verify(r, cookie)
	if err == nil {
		s.sessions.put(cookie.Value, verified)
	}
	return verified, err
}

// verify asks the catalog whom the session cookie belongs to.
func (s *server) verify(r *http.Request, cookie *http.Cookie) (identity, error) {
	defer timed(r.Context(), "auth")()
	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()
	verify, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.AuthVerifyURL, nil)
	if err != nil {
		return identity{}, errAuthUnavailable
	}
	verify.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	// Use the configured origin, never caller-supplied identity or proxy headers.
	verify.Header.Set("X-Forwarded-Host", s.publicOrigin.Host)
	verify.Header.Set("X-Forwarded-Proto", s.publicOrigin.Scheme)
	verify.Header.Set("X-Forwarded-Method", r.Method)
	verify.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())
	response, err := s.authClient.Do(verify)
	if err != nil {
		return identity{}, errAuthUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
		http.StatusUnauthorized, http.StatusForbidden:
		return identity{}, nil
	case http.StatusNoContent:
		subject := strings.TrimSpace(response.Header.Get("X-Temporal-Auth-Subject"))
		email := response.Header.Get("X-Temporal-Auth-Email")
		if subject == "" || email == "" {
			return identity{}, errAuthUnavailable
		}
		if subject == "bootstrap-catalog-auth" || !employeeEmail(email) {
			return identity{}, nil
		}
		return identity{Email: strings.ToLower(email), Name: headerName(response.Header.Get("X-Temporal-Auth-Name"))}, nil
	default:
		return identity{}, errAuthUnavailable
	}
}

// headerName decodes the catalog's name header. Its Python server encodes
// header values as Latin-1, so "Zoë" arrives as bytes that are not UTF-8.
func headerName(value string) string {
	if !utf8.ValidString(value) {
		decoded := make([]rune, len(value))
		for i := 0; i < len(value); i++ {
			decoded[i] = rune(value[i])
		}
		value = string(decoded)
	}
	return strings.TrimSpace(value)
}

// sessionTTL is how long a verified session is trusted without asking the
// catalog again. A session the catalog revokes keeps working that long.
const sessionTTL = 30 * time.Second

// sessionCache remembers recent verifications, keyed by a hash of the cookie,
// so a page and the API calls it makes check the session with the catalog once.
type sessionCache struct {
	mu      sync.Mutex
	entries map[[32]byte]cachedSession
	now     func() time.Time
}

type cachedSession struct {
	identity identity
	expires  time.Time
}

// maxSessions bounds the cache; past it, expired entries go, and if that isn't
// enough, everything does.
const maxSessions = 10000

func newSessionCache() *sessionCache {
	return &sessionCache{entries: map[[32]byte]cachedSession{}, now: time.Now}
}

func (c *sessionCache) get(cookie string) (identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[sha256.Sum256([]byte(cookie))]
	if !ok || !c.now().Before(entry.expires) {
		return identity{}, false
	}
	return entry.identity, true
}

func (c *sessionCache) put(cookie string, verified identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= maxSessions {
		for key, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= maxSessions {
			clear(c.entries)
		}
	}
	c.entries[sha256.Sum256([]byte(cookie))] = cachedSession{identity: verified, expires: now.Add(sessionTTL)}
}

func employeeEmail(email string) bool {
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return false
	}
	local, domain, found := strings.Cut(email, "@")
	return found && local != "" && strings.EqualFold(domain, "temporal.io")
}

func (s *server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		employee, err := s.employee(r)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errAuthUnavailable.Error())
			return
		}
		if employee.Email == "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "Sign in with your Temporal employee account.")
			} else {
				http.Redirect(w, r, s.loginURL("/admin"), http.StatusFound)
			}
			return
		}
		ctx := context.WithValue(r.Context(), employeeContextKey{}, employee.Email)
		next(w, r.WithContext(ctx))
	}
}

// internalParticipant verifies the employee answering an internal-only event.
// It writes the failure response and returns false when there is none; the
// 401 carries a login URL that returns to the event's link.
func (s *server) internalParticipant(w http.ResponseWriter, r *http.Request, event tracker.Event) (identity, bool) {
	participant, err := s.employee(r)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errAuthUnavailable.Error())
		return identity{}, false
	}
	if participant.Email == "" {
		writeJSON(w, http.StatusUnauthorized, struct {
			Error    string `json:"error"`
			LoginURL string `json:"loginUrl"`
		}{"This form is for Temporal employees. Sign in with your Temporal account to respond.", s.loginURL("/" + event.Link())})
		return identity{}, false
	}
	return participant, true
}
