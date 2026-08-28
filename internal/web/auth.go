package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie = "bmn_session"
	csrfCookie    = "bmn_csrf"
	flashCookie   = "bmn_flash"
	sessionTTL    = 30 * 24 * time.Hour
)

// sign returns the HMAC of a value using the app secret.
func (s *Server) sign(value string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// setSession issues the organizer's login cookie. The cookie is a signed expiry
// rather than a database row: there is exactly one account, so statelessness
// costs nothing and logout is just clearing the cookie.
func (s *Server) setSession(w http.ResponseWriter) {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    exp + "." + s.sign(exp),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", HttpOnly: true,
		Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// loggedIn reports whether the request carries a valid, unexpired session.
func (s *Server) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	if !hmac.Equal([]byte(sig), []byte(s.sign(exp))) {
		return false
	}
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < ts
}

// requireAuth guards the organizer routes.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loggedIn(r) {
			next := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+urlQueryEscape(next), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// csrfToken returns the double-submit token, creating it if needed. The cookie
// stays HttpOnly because the server writes the matching hidden field itself.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	return token
}

// checkCSRF validates an organizer form submission.
func (s *Server) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	got := r.PostFormValue("csrf")
	return got != "" && hmac.Equal([]byte(got), []byte(c.Value))
}

// flash is a one-shot message shown after a redirect.
type flash struct {
	Kind    string // ok | error
	Message string
}

// IsError reports whether the flash should be styled as a problem.
func (f flash) IsError() bool { return f.Kind == "error" }

func setFlash(w http.ResponseWriter, secure bool, kind, msg string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + msg))
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: v, Path: "/", HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: 60,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(string(raw), "|")
	if !ok {
		return nil
	}
	return &flash{Kind: kind, Message: msg}
}
