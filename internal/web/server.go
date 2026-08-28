package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"badminton/internal/config"
	"badminton/internal/mail"
	"badminton/internal/store"
)

// Server holds the HTTP layer's dependencies.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	worker    *mail.Worker
	log       *slog.Logger
	templates map[string]*template.Template

	secret        []byte
	secureCookies bool
	trustProxy    bool
	loginLimit    *limiter
	publicLimit   *limiter

	assetV string

	// now is injectable so tests can move past a signup deadline.
	now func() time.Time
}

// New builds the HTTP server. The cookie signing secret is generated once and
// stored in the database, so logins survive restarts without another env var.
func New(cfg *config.Config, st *store.Store, worker *mail.Worker, log *slog.Logger) (*Server, error) {
	secret, err := st.MetaOrCreate(context.Background(), "cookie_secret", func() string {
		b := make([]byte, 32)
		rand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	})
	if err != nil {
		return nil, fmt.Errorf("cookie secret: %w", err)
	}

	s := &Server{
		cfg:           cfg,
		store:         st,
		worker:        worker,
		log:           log,
		secret:        []byte(secret),
		secureCookies: strings.HasPrefix(cfg.BaseURL, "https://"),
		trustProxy:    config.TrustProxy(),
		// Ten login attempts, refilling slowly: enough for a fat-fingered
		// password, useless for guessing one.
		loginLimit:  newLimiter(10, 1.0/30.0),
		publicLimit: newLimiter(60, 1),
		assetV:      assetVersion(),
		now:         time.Now,
	}
	if err := s.buildTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the fully wired router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Participant routes. The token in the path is the only credential, so
	// these are rate limited and kept out of search engines.
	mux.HandleFunc("GET /r/{token}", s.publicLimited(s.handleRSVP))
	mux.HandleFunc("POST /r/{token}/signup", s.publicLimited(s.handleRSVPSignup))
	mux.HandleFunc("POST /r/{token}/guest", s.publicLimited(s.handleRSVPGuest))
	mux.HandleFunc("POST /r/{token}/cancel", s.publicLimited(s.handleRSVPCancel))
	mux.HandleFunc("GET /s/{publicID}", s.publicLimited(s.handlePublicSignup))
	mux.HandleFunc("POST /s/{publicID}", s.publicLimited(s.handlePublicSignupPost))
	mux.HandleFunc("GET /u/{token}", s.publicLimited(s.handleUnsubscribe))
	mux.HandleFunc("POST /u/{token}", s.publicLimited(s.handleUnsubscribePost))

	// Organizer routes.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.requireAuth(s.handleDashboard))

	mux.HandleFunc("GET /sessions/new", s.requireAuth(s.handleSessionForm))
	mux.HandleFunc("POST /sessions/new", s.requireAuth(s.guard(s.handleSessionCreate)))
	mux.HandleFunc("GET /sessions/{id}", s.requireAuth(s.handleSessionDetail))
	mux.HandleFunc("GET /sessions/{id}/edit", s.requireAuth(s.handleSessionForm))
	mux.HandleFunc("POST /sessions/{id}/edit", s.requireAuth(s.guard(s.handleSessionUpdate)))
	mux.HandleFunc("POST /sessions/{id}/invite", s.requireAuth(s.guard(s.handleSendInvitation)))
	mux.HandleFunc("POST /sessions/{id}/remind", s.requireAuth(s.guard(s.handleSendReminder)))
	mux.HandleFunc("POST /sessions/{id}/promote", s.requireAuth(s.guard(s.handlePromote)))
	mux.HandleFunc("POST /sessions/{id}/status", s.requireAuth(s.guard(s.handleSessionStatus)))
	mux.HandleFunc("POST /sessions/{id}/courts", s.requireAuth(s.guard(s.handleSetCourts)))
	mux.HandleFunc("POST /sessions/{id}/participants", s.requireAuth(s.guard(s.handleAddParticipant)))
	mux.HandleFunc("POST /sessions/{id}/participants/{rsvpID}/remove", s.requireAuth(s.guard(s.handleRemoveParticipant)))

	mux.HandleFunc("GET /players", s.requireAuth(s.handlePlayers))
	mux.HandleFunc("POST /players/new", s.requireAuth(s.guard(s.handlePlayerCreate)))
	mux.HandleFunc("POST /players/{id}/edit", s.requireAuth(s.guard(s.handlePlayerUpdate)))
	mux.HandleFunc("POST /players/{id}/delete", s.requireAuth(s.guard(s.handlePlayerDelete)))
	mux.HandleFunc("POST /players/import", s.requireAuth(s.guard(s.handlePlayerImport)))

	mux.HandleFunc("GET /settings", s.requireAuth(s.handleSettings))
	mux.HandleFunc("POST /settings", s.requireAuth(s.guard(s.handleSettingsSave)))
	mux.HandleFunc("POST /settings/test-email", s.requireAuth(s.guard(s.handleTestEmail)))

	mux.HandleFunc("GET /outbox", s.requireAuth(s.handleOutbox))
	mux.HandleFunc("POST /outbox/{id}/retry", s.requireAuth(s.guard(s.handleOutboxRetry)))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})

	if sub, err := StaticFS(); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/",
			cacheControl(http.FileServer(http.FS(sub)))))
	}

	return s.securityHeaders(s.recoverPanic(s.logRequests(mux)))
}

// guard wraps a state-changing organizer handler with CSRF validation.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CSV upload posts multipart, which ParseForm does not read; without
		// this the CSRF field would appear missing on every import.
		var err error
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			err = r.ParseMultipartForm(4 << 20)
		} else {
			err = r.ParseForm()
		}
		if err != nil {
			s.fail(w, r, "That form could not be read.", "/")
			return
		}
		if !s.checkCSRF(r) {
			s.log.Warn("csrf check failed", "path", r.URL.Path, "ip", clientIP(r, s.trustProxy))
			s.fail(w, r, "Your session expired. Please try again.", r.Referer())
			return
		}
		next(w, r)
	}
}

// publicLimited rate limits the token-based participant routes.
func (s *Server) publicLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.publicLimit.allow(clientIP(r, s.trustProxy)) {
			http.Error(w, "Too many requests. Please wait a moment and try again.", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
		"script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", rec)
				http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// Tokens live in the path, so log the route shape rather than the URL.
		s.log.Debug("request", "method", r.Method, "path", redactPath(r.URL.Path),
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// redactPath strips participant tokens out of log lines. A token in a log file
// is a working credential for someone else's RSVP.
func redactPath(p string) string {
	for _, prefix := range []string{"/r/", "/u/", "/s/"} {
		if strings.HasPrefix(p, prefix) {
			rest := strings.TrimPrefix(p, prefix)
			if i := strings.Index(rest, "/"); i >= 0 {
				return prefix + "[token]" + rest[i:]
			}
			return prefix + "[token]"
		}
	}
	return p
}

func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Safe to cache hard because the URLs carry a content hash.
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		next.ServeHTTP(w, r)
	})
}

// ok redirects with a success flash.
func (s *Server) ok(w http.ResponseWriter, r *http.Request, msg, to string) {
	setFlash(w, s.secureCookies, "ok", msg)
	s.redirect(w, r, to)
}

// fail redirects with an error flash.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg, to string) {
	setFlash(w, s.secureCookies, "error", msg)
	s.redirect(w, r, to)
}

// redirect sends the browser onward, refusing off-site targets so a Referer
// header can never bounce a user somewhere else.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to string) {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		to = "/"
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("handler error", "path", redactPath(r.URL.Path), "err", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.renderPublicStatus(w, r, "notfound.html", view{Title: "Not found"}, http.StatusNotFound)
}
