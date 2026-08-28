// Package web contains the HTTP handlers, templates and middleware. Every page
// is server-rendered and every form is a plain POST-and-redirect, so the app
// works without JavaScript and needs no build step.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"badminton/internal/model"
)

//go:embed templates/*.html templates/pages/*.html static/*
var assetFS embed.FS

// StaticFS exposes the embedded static files for serving.
func StaticFS() (fs.FS, error) { return fs.Sub(assetFS, "static") }

// assetVersion hashes the embedded static files so their URLs change whenever
// they do. Without it, the hour-long cache header means a deployed CSS fix does
// not reach anyone still holding the old copy.
func assetVersion() string {
	h := sha256.New()
	names, err := fs.Glob(assetFS, "static/*")
	if err != nil {
		return "0"
	}
	for _, name := range names {
		b, err := assetFS.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write([]byte(name))
		h.Write(b)
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:10]
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"money": model.FormatCents,
		// dateLine renders a session's YYYY-MM-DD as "Tuesday, September 1".
		"dateLine": func(sess model.Session) string {
			return sess.StartAt(s.cfg.Timezone).Format("Monday, January 2")
		},
		"shortDate": func(sess model.Session) string {
			return sess.StartAt(s.cfg.Timezone).Format("Mon 2 Jan")
		},
		"timeRange": func(sess model.Session) string {
			return sess.StartAt(s.cfg.Timezone).Format("3:04 PM") + " - " +
				sess.EndAt(s.cfg.Timezone).Format("3:04 PM")
		},
		"stamp": func(unix int64) string {
			return time.Unix(unix, 0).In(s.cfg.Timezone).Format("Mon 2 Jan, 3:04 PM")
		},
		"stampPtr": func(unix *int64) string {
			if unix == nil {
				return "never"
			}
			return time.Unix(*unix, 0).In(s.cfg.Timezone).Format("Mon 2 Jan, 3:04 PM")
		},
		"dateInput": func(unix int64) string {
			return time.Unix(unix, 0).In(s.cfg.Timezone).Format("2006-01-02")
		},
		"timeInput": func(unix int64) string {
			return time.Unix(unix, 0).In(s.cfg.Timezone).Format("15:04")
		},
		"since": humanSince,
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		"seq": func(n int) []int {
			out := make([]int, 0, n)
			for i := 1; i <= n; i++ {
				out = append(out, i)
			}
			return out
		},
		"add": func(a, b int) int { return a + b },
		"dollars": func(cents int64) string {
			return fmt.Sprintf("%d.%02d", cents/100, cents%100)
		},
		"statusClass": func(status string) string {
			return "badge badge-" + strings.ToLower(status)
		},
	}
}

// buildTemplates parses each page together with the shared layouts. Parsing per
// page keeps the "content" block from colliding between pages.
func (s *Server) buildTemplates() error {
	pages, err := fs.Glob(assetFS, "templates/pages/*.html")
	if err != nil {
		return err
	}
	s.templates = map[string]*template.Template{}
	for _, page := range pages {
		name := strings.TrimPrefix(page, "templates/pages/")
		t, err := template.New(name).Funcs(s.funcs()).ParseFS(assetFS, "templates/*.html", page)
		if err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		s.templates[name] = t
	}
	return nil
}

// view is the data handed to every template. Page-specific values go in Data.
type view struct {
	Title       string
	Page        string
	Flash       *flash
	CSRF        string
	LoggedIn    bool
	Organizer   string
	BaseURL     string
	AssetV      string
	PendingMail int
	Data        any
}

// render writes a page using the shared layout.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page, layout string, v view) {
	t, ok := s.templates[page]
	if !ok {
		s.log.Error("template not found", "page", page)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	v.Flash = takeFlash(w, r)
	v.BaseURL = s.cfg.BaseURL
	v.AssetV = s.assetV
	if v.LoggedIn {
		v.CSRF = s.csrfToken(w, r)
		v.Organizer = s.cfg.OrganizerName
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, layout, v); err != nil {
		// The status line is already written by now, so all we can do is log.
		s.log.Error("render failed", "page", page, "err", err)
	}
}

// renderAdmin renders an organizer page.
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, page string, v view) {
	v.LoggedIn = true
	if n, err := s.store.PendingCount(r.Context()); err == nil {
		v.PendingMail = n
	}
	s.render(w, r, page, "admin.html", v)
}

// renderPublic renders a participant-facing page with the minimal layout.
func (s *Server) renderPublic(w http.ResponseWriter, r *http.Request, page string, v view) {
	// These pages hang off unguessable tokens: keep them out of search indexes
	// and out of referrer headers so the token cannot leak sideways.
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.render(w, r, page, "public.html", v)
}

func humanSince(unix int64) string {
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
