package web

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

const authTimeout = 3 * time.Second

var errAuthUnavailable = errors.New("Employee authentication is unavailable. Please try again.")

type employeeContextKey struct{}

// An empty email with no error means the visitor is not authenticated.
func (s *server) employee(r *http.Request) (string, error) {
	if s.config.DevAuthEmail != "" {
		return strings.ToLower(s.config.DevAuthEmail), nil
	}
	cookie, err := r.Cookie("temporal_demo_auth")
	if err != nil || cookie.Value == "" || len(cookie.Value) > 8192 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), authTimeout)
	defer cancel()
	verify, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.AuthVerifyURL, nil)
	if err != nil {
		return "", errAuthUnavailable
	}
	verify.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	// Use the configured origin, never caller-supplied identity or proxy headers.
	verify.Header.Set("X-Forwarded-Host", s.publicOrigin.Host)
	verify.Header.Set("X-Forwarded-Proto", s.publicOrigin.Scheme)
	verify.Header.Set("X-Forwarded-Method", r.Method)
	verify.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())
	response, err := s.authClient.Do(verify)
	if err != nil {
		return "", errAuthUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
		http.StatusUnauthorized, http.StatusForbidden:
		return "", nil
	case http.StatusNoContent:
		subject := strings.TrimSpace(response.Header.Get("X-Temporal-Auth-Subject"))
		email := response.Header.Get("X-Temporal-Auth-Email")
		if subject == "" || email == "" {
			return "", errAuthUnavailable
		}
		if subject == "bootstrap-catalog-auth" || !employeeEmail(email) {
			return "", nil
		}
		return strings.ToLower(email), nil
	default:
		return "", errAuthUnavailable
	}
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
		email, err := s.employee(r)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, errAuthUnavailable.Error())
			return
		}
		if email == "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "Sign in with your Temporal employee account.")
			} else {
				http.Redirect(w, r, s.loginURL(), http.StatusFound)
			}
			return
		}
		ctx := context.WithValue(r.Context(), employeeContextKey{}, email)
		next(w, r.WithContext(ctx))
	}
}
