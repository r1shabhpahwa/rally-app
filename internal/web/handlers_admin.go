package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"badminton/internal/mail"
	"badminton/internal/model"
	"badminton/internal/store"
)

// summary pairs a session with its computed roster, status and cost, which is
// what every list and detail view needs.
type summary struct {
	Session model.Session
	Roster  model.Roster
	Status  string
	Cost    model.Cost
}

func (s *Server) summarize(ctx context.Context, sessions []model.Session) ([]summary, error) {
	out := make([]summary, 0, len(sessions))
	for _, sess := range sessions {
		r, err := s.store.Roster(ctx, sess)
		if err != nil {
			return nil, err
		}
		out = append(out, summary{
			Session: sess,
			Roster:  r,
			Status:  r.DisplayStatus(s.now(), s.cfg.Timezone),
			Cost:    sess.Cost(r.Headcount),
		})
	}
	return out, nil
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.loggedIn(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login.html", "public.html", view{
		Title: "Sign in",
		Data:  map[string]any{"Next": r.URL.Query().Get("next")},
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "That form could not be read.", "/login")
		return
	}
	if !s.loginLimit.allow(clientIP(r, s.trustProxy)) {
		s.fail(w, r, "Too many attempts. Please wait a minute and try again.", "/login")
		return
	}

	org, err := s.store.Organizer(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	password := r.PostFormValue("password")
	if org == nil || !org.CheckPassword(password) {
		s.log.Warn("failed login", "ip", clientIP(r, s.trustProxy))
		s.fail(w, r, "That password is not right.", "/login")
		return
	}

	s.setSession(w)
	next := r.PostFormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, sessionCookie)
	s.ok(w, r, "Signed out.", "/login")
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	today := s.now().In(s.cfg.Timezone).Format("2006-01-02")

	upcomingSessions, err := s.store.Sessions(ctx, today, false)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pastSessions, err := s.store.Sessions(ctx, today, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(pastSessions) > 20 {
		pastSessions = pastSessions[:20]
	}
	upcoming, err := s.summarize(ctx, upcomingSessions)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	past, err := s.summarize(ctx, pastSessions)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mailingList, err := s.store.MailablePlayers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	last, err := s.store.LatestSession(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	s.renderAdmin(w, r, "dashboard.html", view{
		Title: "Sessions",
		Page:  "sessions",
		Data: map[string]any{
			"Upcoming":     upcoming,
			"Past":         past,
			"MailingCount": len(mailingList),
			"CanDuplicate": last != nil,
		},
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	defaults, err := s.store.Defaults(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderAdmin(w, r, "settings.html", view{
		Title: "Settings",
		Page:  "settings",
		Data: map[string]any{
			"Defaults":      defaults,
			"SMTPHost":      s.cfg.SMTP.Host,
			"SMTPFrom":      s.cfg.SMTP.From,
			"SMTPEnabled":   s.cfg.SMTP.Enabled(),
			"Timezone":      s.cfg.Timezone.String(),
			"BaseURL":       s.cfg.BaseURL,
			"OrganizerMail": s.cfg.OrganizerEmail,
		},
	})
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	defaults, err := s.store.Defaults(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	cost, err := formCents(r, "cost")
	if err != nil {
		s.fail(w, r, "Cost: "+err.Error(), "/settings")
		return
	}
	start, err := validClock(r.PostFormValue("start_time"))
	if err != nil {
		s.fail(w, r, "Start time: "+err.Error(), "/settings")
		return
	}
	end, err := validClock(r.PostFormValue("end_time"))
	if err != nil {
		s.fail(w, r, "End time: "+err.Error(), "/settings")
		return
	}
	deadlineTime, err := validClock(r.PostFormValue("deadline_time"))
	if err != nil {
		s.fail(w, r, "Deadline time: "+err.Error(), "/settings")
		return
	}

	defaults.Venue = strings.TrimSpace(r.PostFormValue("venue"))
	defaults.Courts = formInt(r, "courts", defaults.Courts)
	defaults.CostPerCourtHourCents = cost
	defaults.MaxPlayers = formInt(r, "max_players", defaults.MaxPlayers)
	defaults.StartTime = start
	defaults.EndTime = end
	defaults.DeadlineDaysBefore = formInt(r, "deadline_days_before", defaults.DeadlineDaysBefore)
	defaults.DeadlineTime = deadlineTime
	defaults.ThingsToBring = strings.TrimSpace(r.PostFormValue("things_to_bring"))

	if defaults.Courts < 1 || defaults.MaxPlayers < 1 {
		s.fail(w, r, "Courts and maximum players must be at least 1.", "/settings")
		return
	}
	if err := s.store.SaveDefaults(ctx, defaults); err != nil {
		s.serverError(w, r, err)
		return
	}

	if pw := r.PostFormValue("new_password"); pw != "" {
		if len(pw) < 8 {
			s.fail(w, r, "Defaults saved, but the new password must be at least 8 characters.", "/settings")
			return
		}
		org, err := s.store.Organizer(ctx)
		if err != nil || org == nil {
			s.serverError(w, r, err)
			return
		}
		if !org.CheckPassword(r.PostFormValue("current_password")) {
			s.fail(w, r, "Defaults saved, but the current password was wrong so it was not changed.", "/settings")
			return
		}
		if err := s.store.SetOrganizerPassword(ctx, org.ID, pw); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.ok(w, r, "Settings saved and password changed.", "/settings")
		return
	}
	s.ok(w, r, "Settings saved.", "/settings")
}

func (s *Server) handleTestEmail(w http.ResponseWriter, r *http.Request) {
	to := strings.TrimSpace(r.PostFormValue("to"))
	if to == "" {
		to = s.cfg.OrganizerEmail
	}
	if !looksLikeEmail(to) {
		s.fail(w, r, "That does not look like an email address.", "/settings")
		return
	}
	msg, err := mail.TestMessage(s.cfg.BaseURL, s.cfg.OrganizerName, to)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.Enqueue(r.Context(), msg); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.worker.Wake()
	s.ok(w, r, "Test email queued for "+to+". Check the email log for the result.", "/outbox")
}

func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.RecentMessages(r.Context(), 200)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderAdmin(w, r, "outbox.html", view{
		Title: "Email log",
		Page:  "outbox",
		Data:  map[string]any{"Messages": msgs},
	})
}

func (s *Server) handleOutboxRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	if err := s.store.RetryMessage(r.Context(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.worker.Wake()
	s.ok(w, r, "Message queued for another attempt.", "/outbox")
}

// mailContext assembles everything the email composers need for a session.
func (s *Server) mailContext(ctx context.Context, sess model.Session, roster model.Roster) (mail.Context, error) {
	defaults, err := s.store.Defaults(ctx)
	if err != nil {
		return mail.Context{}, err
	}
	return mail.Context{
		Session:        sess,
		Roster:         roster,
		Loc:            s.cfg.Timezone,
		BaseURL:        s.cfg.BaseURL,
		ThingsToBring:  defaults.ThingsToBring,
		OrganizerName:  s.cfg.OrganizerName,
		OrganizerEmail: s.cfg.OrganizerEmail,
	}, nil
}

// organizerAsPlayer gives the organizer a Player shape so notices can go
// through the same composer as everything else.
func (s *Server) organizerAsPlayer() model.Player {
	return model.Player{Name: s.cfg.OrganizerName, Email: s.cfg.OrganizerEmail}
}

var _ = store.ErrNotFound
