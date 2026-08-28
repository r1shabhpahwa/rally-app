package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"badminton/internal/mail"
	"badminton/internal/model"
	"badminton/internal/store"
)

// loadSession resolves the {id} path value, writing a 404 if it is missing.
func (s *Server) loadSession(w http.ResponseWriter, r *http.Request) (*model.Session, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return nil, false
	}
	sess, err := s.store.Session(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return nil, false
	}
	if sess == nil {
		s.notFound(w, r)
		return nil, false
	}
	return sess, true
}

// handleSessionForm serves both the new-session and edit forms. A new session
// is prefilled from the saved defaults, or from last week's session when
// duplicating, so the weekly job is a couple of clicks.
func (s *Server) handleSessionForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	defaults, err := s.store.Defaults(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var (
		sess      model.Session
		deadlines []model.CourtDeadline
		editing   bool
	)

	if idStr := r.PathValue("id"); idStr != "" {
		existing, ok := s.loadSession(w, r)
		if !ok {
			return
		}
		sess, editing = *existing, true
		deadlines, err = s.store.CourtDeadlines(ctx, sess.ID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	} else {
		sess = model.Session{
			Venue:                 defaults.Venue,
			Courts:                defaults.Courts,
			CostPerCourtHourCents: defaults.CostPerCourtHourCents,
			MaxPlayers:            defaults.MaxPlayers,
			StartTime:             defaults.StartTime,
			EndTime:               defaults.EndTime,
		}
		next := s.now().In(s.cfg.Timezone).AddDate(0, 0, 7)

		if r.URL.Query().Get("duplicate") != "" {
			last, err := s.store.LatestSession(ctx)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			if last != nil {
				sess.Venue = last.Venue
				sess.Courts = last.Courts
				sess.CostPerCourtHourCents = last.CostPerCourtHourCents
				sess.MaxPlayers = last.MaxPlayers
				sess.StartTime = last.StartTime
				sess.EndTime = last.EndTime
				sess.Notes = last.Notes
				// Same slot, one week on: that is what "duplicate" means here.
				next = last.StartAt(s.cfg.Timezone).AddDate(0, 0, 7)
			}
		}
		sess.Date = next.Format("2006-01-02")
		sess.SignupDeadlineAt = defaultDeadline(next, defaults, s.cfg.Timezone).Unix()
	}

	s.renderAdmin(w, r, "session_form.html", view{
		Title: formTitle(editing),
		Page:  "sessions",
		Data: map[string]any{
			"Session":   sess,
			"Deadlines": deadlineMap(deadlines),
			"Editing":   editing,
			"Action":    formAction(editing, sess.ID),
		},
	})
}

func formTitle(editing bool) string {
	if editing {
		return "Edit session"
	}
	return "New session"
}

func formAction(editing bool, id int64) string {
	if editing {
		return fmt.Sprintf("/sessions/%d/edit", id)
	}
	return "/sessions/new"
}

func deadlineMap(ds []model.CourtDeadline) map[int]int64 {
	m := map[int]int64{}
	for _, d := range ds {
		m[d.CourtNumber] = d.DeadlineAt
	}
	return m
}

// defaultDeadline puts the signup cut-off the configured number of days before
// the session, at the configured time.
func defaultDeadline(sessionStart time.Time, d store.Defaults, loc *time.Location) time.Time {
	day := sessionStart.In(loc).AddDate(0, 0, -d.DeadlineDaysBefore)
	clock, err := time.Parse("15:04", d.DeadlineTime)
	if err != nil {
		clock, _ = time.Parse("15:04", "15:00")
	}
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
}

// parseSessionForm reads and validates the session form.
func (s *Server) parseSessionForm(r *http.Request) (model.Session, []model.CourtDeadline, error) {
	var sess model.Session

	date, err := validDate(r.PostFormValue("date"))
	if err != nil {
		return sess, nil, fmt.Errorf("date: %w", err)
	}
	start, err := validClock(r.PostFormValue("start_time"))
	if err != nil {
		return sess, nil, fmt.Errorf("start time: %w", err)
	}
	end, err := validClock(r.PostFormValue("end_time"))
	if err != nil {
		return sess, nil, fmt.Errorf("end time: %w", err)
	}
	cost, err := formCents(r, "cost")
	if err != nil {
		return sess, nil, fmt.Errorf("cost: %w", err)
	}
	deadline, err := localTime(r.PostFormValue("deadline_date"), r.PostFormValue("deadline_time"), s.cfg.Timezone)
	if err != nil {
		return sess, nil, fmt.Errorf("signup deadline: %w", err)
	}

	sess = model.Session{
		Date: date, StartTime: start, EndTime: end,
		Venue:                 strings.TrimSpace(r.PostFormValue("venue")),
		Courts:                formInt(r, "courts", 0),
		CostPerCourtHourCents: cost,
		MaxPlayers:            formInt(r, "max_players", 0),
		SignupDeadlineAt:      deadline.Unix(),
		Notes:                 strings.TrimSpace(r.PostFormValue("notes")),
	}
	if sess.Courts < 1 {
		return sess, nil, errors.New("there must be at least one court")
	}
	if sess.MaxPlayers < 1 {
		return sess, nil, errors.New("maximum players must be at least 1")
	}
	if sess.Minutes() <= 0 {
		return sess, nil, errors.New("the end time must be after the start time")
	}
	if !deadline.Before(sess.StartAt(s.cfg.Timezone)) {
		return sess, nil, errors.New("the signup deadline must be before the session starts")
	}

	var deadlines []model.CourtDeadline
	for court := 1; court <= sess.Courts; court++ {
		d := r.PostFormValue(fmt.Sprintf("court_deadline_date_%d", court))
		t := r.PostFormValue(fmt.Sprintf("court_deadline_time_%d", court))
		if strings.TrimSpace(d) == "" || strings.TrimSpace(t) == "" {
			continue
		}
		at, err := localTime(d, t, s.cfg.Timezone)
		if err != nil {
			return sess, nil, fmt.Errorf("court %d cancellation deadline: %w", court, err)
		}
		deadlines = append(deadlines, model.CourtDeadline{CourtNumber: court, DeadlineAt: at.Unix()})
	}
	return sess, deadlines, nil
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	sess, deadlines, err := s.parseSessionForm(r)
	if err != nil {
		s.fail(w, r, capitalise(err.Error())+".", "/sessions/new")
		return
	}
	// Sessions open immediately: a draft would hand out signup links that
	// refuse every click. Draft stays available from the status control.
	sess.Status = model.SessionOpen
	if r.PostFormValue("draft") != "" {
		sess.Status = model.SessionDraft
	}

	created, err := s.store.CreateSession(r.Context(), sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(deadlines) > 0 {
		if err := s.store.ReplaceCourtDeadlines(r.Context(), created.ID, deadlines); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.ok(w, r, "Session created. Send the invitation when you are ready.",
		fmt.Sprintf("/sessions/%d", created.ID))
}

func (s *Server) handleSessionUpdate(w http.ResponseWriter, r *http.Request) {
	existing, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	sess, deadlines, err := s.parseSessionForm(r)
	if err != nil {
		s.fail(w, r, capitalise(err.Error())+".", fmt.Sprintf("/sessions/%d/edit", existing.ID))
		return
	}
	sess.ID = existing.ID
	if err := s.store.UpdateSession(r.Context(), sess); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.ReplaceCourtDeadlines(r.Context(), existing.ID, deadlines); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.ok(w, r, "Session updated.", fmt.Sprintf("/sessions/%d", existing.ID))
}

func (s *Server) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	deadlines, err := s.store.CourtDeadlines(ctx, sess.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mailStats, err := s.store.SessionMailStats(ctx, sess.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	everyone, err := s.store.Players(ctx, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// People already on the roster should not appear in the "add someone" list.
	onRoster := map[int64]bool{}
	for _, e := range roster.Confirmed {
		onRoster[e.PlayerID] = true
	}
	for _, e := range roster.Waitlist {
		onRoster[e.PlayerID] = true
	}
	addable := everyone[:0:0]
	for _, p := range everyone {
		if !onRoster[p.ID] {
			addable = append(addable, p)
		}
	}

	_, skipped := roster.NextPromotable()
	now := s.now()

	s.renderAdmin(w, r, "session_detail.html", view{
		Title: "Session",
		Page:  "sessions",
		Data: map[string]any{
			"Session":      *sess,
			"Roster":       roster,
			"Status":       roster.DisplayStatus(now, s.cfg.Timezone),
			"Cost":         sess.Cost(roster.Headcount),
			"Deadlines":    deadlines,
			"MailStats":    mailStats,
			"Addable":      addable,
			"SignupsOpen":  roster.SignupsOpen(now, s.cfg.Timezone),
			"PromoteSkips": skipped,
			"PublicURL":    s.cfg.BaseURL + "/s/" + sess.PublicID,
			"DeadlinePast": now.After(sess.SignupDeadline()),
			"NoResponse":   roster.Invited,
		},
	})
}

func (s *Server) handleSendInvitation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	to := fmt.Sprintf("/sessions/%d", sess.ID)

	// Guard against a double click turning 32 emails into 64.
	if sess.InvitationSentAt != nil && r.PostFormValue("confirm_resend") == "" {
		s.fail(w, r, "The invitation has already been sent. Tick the resend box if you really want to send it again.", to)
		return
	}

	players, err := s.store.MailablePlayers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(players) == 0 {
		s.fail(w, r, "The mailing list is empty. Add people first.", "/players")
		return
	}

	entries, err := s.store.EnsureInvites(ctx, sess.ID, players)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mc, err := s.mailContext(ctx, *sess, roster)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	tokens := tokensByPlayer(entries)
	msgs := make([]model.OutboxMessage, 0, len(players))
	for _, p := range players {
		token, ok := tokens[p.ID]
		if !ok {
			continue
		}
		msg, err := mc.Invitation(p, token)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		msgs = append(msgs, msg)
	}
	if err := s.store.Enqueue(ctx, msgs...); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.MarkInvitationSent(ctx, sess.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// A draft that has just been invited must accept signups, or every link in
	// the email that just went out is dead.
	if sess.Status == model.SessionDraft {
		if err := s.store.SetSessionStatus(ctx, sess.ID, model.SessionOpen); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	s.worker.Wake()
	s.ok(w, r, fmt.Sprintf("Invitation queued for %d %s.", len(msgs), plural(len(msgs), "person", "people")), to)
}

func (s *Server) handleSendReminder(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	to := fmt.Sprintf("/sessions/%d", sess.ID)

	players, err := s.store.MailablePlayers(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Top up anyone added to the mailing list after the invitation went out, so
	// they get a working link rather than nothing.
	entries, err := s.store.EnsureInvites(ctx, sess.ID, players)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mc, err := s.mailContext(ctx, *sess, roster)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	includeAll := r.PostFormValue("include_all") != ""
	answered := map[int64]bool{}
	for _, e := range entries {
		if e.Status == model.StatusConfirmed || e.Status == model.StatusWaitlist {
			answered[e.PlayerID] = true
		}
	}

	tokens := tokensByPlayer(entries)
	msgs := make([]model.OutboxMessage, 0, len(players))
	for _, p := range players {
		if !includeAll && answered[p.ID] {
			continue
		}
		token, ok := tokens[p.ID]
		if !ok {
			continue
		}
		msg, err := mc.Reminder(p, token)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) == 0 {
		s.fail(w, r, "Everyone on the list has already replied. Tick \"send to everyone\" to remind them anyway.", to)
		return
	}
	if err := s.store.Enqueue(ctx, msgs...); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.MarkReminderSent(ctx, sess.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.worker.Wake()
	s.ok(w, r, fmt.Sprintf("Reminder queued for %d %s.", len(msgs), plural(len(msgs), "person", "people")), to)
}

func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	to := fmt.Sprintf("/sessions/%d", sess.ID)

	promoted, skipped, err := s.store.PromoteNext(ctx, sess.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if promoted == nil {
		if len(skipped) > 0 {
			s.fail(w, r, fmt.Sprintf("%s needs %d spots and there are not that many free. Cancel a court or free more spots first.",
				skipped[0].PlayerName, skipped[0].PartySize()), to)
			return
		}
		s.fail(w, r, "Nobody is on the waitlist.", to)
		return
	}

	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mc, err := s.mailContext(ctx, *sess, roster)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	player, err := s.store.Player(ctx, promoted.PlayerID)
	if err != nil || player == nil {
		s.serverError(w, r, err)
		return
	}
	msg, err := mc.Promoted(*player, *promoted)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.store.Enqueue(ctx, msg); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.worker.Wake()

	note := fmt.Sprintf("%s is confirmed and has been emailed.", promoted.PlayerName)
	if len(skipped) > 0 {
		names := make([]string, 0, len(skipped))
		for _, e := range skipped {
			names = append(names, fmt.Sprintf("%s (needs %d)", e.PlayerName, e.PartySize()))
		}
		note += " Skipped ahead of " + strings.Join(names, ", ") + " — not enough room for their party."
	}
	s.ok(w, r, note, to)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	to := fmt.Sprintf("/sessions/%d", sess.ID)

	status := r.PostFormValue("status")
	switch status {
	case model.SessionDraft, model.SessionOpen, model.SessionClosed, model.SessionCancelled:
	default:
		s.fail(w, r, "That is not a valid status.", to)
		return
	}
	if err := s.store.SetSessionStatus(ctx, sess.ID, status); err != nil {
		s.serverError(w, r, err)
		return
	}

	if status != model.SessionCancelled {
		s.ok(w, r, "Session marked "+status+".", to)
		return
	}

	// Cancelling has to tell the people who were coming.
	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	mc, err := s.mailContext(ctx, *sess, roster)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var msgs []model.OutboxMessage
	for _, e := range append(append([]model.Entry{}, roster.Confirmed...), roster.Waitlist...) {
		player, err := s.store.Player(ctx, e.PlayerID)
		if err != nil || player == nil {
			continue
		}
		msg, err := mc.Cancelled(*player)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) > 0 {
		if err := s.store.Enqueue(ctx, msgs...); err != nil {
			s.serverError(w, r, err)
			return
		}
		s.worker.Wake()
	}
	s.ok(w, r, fmt.Sprintf("Session cancelled. %d %s notified.", len(msgs), plural(len(msgs), "person", "people")), to)
}

func (s *Server) handleSetCourts(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	to := fmt.Sprintf("/sessions/%d", sess.ID)
	courts := formInt(r, "courts", sess.Courts)
	if courts < 1 {
		s.fail(w, r, "There must be at least one court.", to)
		return
	}
	if err := s.store.SetCourts(r.Context(), sess.ID, courts); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.ok(w, r, fmt.Sprintf("Now booked for %d %s. The cost split has been updated.",
		courts, plural(courts, "court", "courts")), to)
}

// handleAddParticipant is the escape hatch for the person who texts instead of
// clicking the link.
func (s *Server) handleAddParticipant(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	to := fmt.Sprintf("/sessions/%d", sess.ID)

	var playerID int64
	if v := strings.TrimSpace(r.PostFormValue("player_id")); v != "" && v != "new" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.fail(w, r, "Pick someone from the list or enter a new name and email.", to)
			return
		}
		playerID = id
	} else {
		name := strings.TrimSpace(r.PostFormValue("name"))
		email := strings.TrimSpace(r.PostFormValue("email"))
		if name == "" || !looksLikeEmail(email) {
			s.fail(w, r, "A name and a valid email address are both needed.", to)
			return
		}
		existing, err := s.store.PlayerByEmail(ctx, email)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if existing != nil {
			playerID = existing.ID
		} else {
			created, err := s.store.CreatePlayer(ctx, name, email, r.PostFormValue("add_to_list") != "")
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			playerID = created.ID
		}
	}

	guests := formInt(r, "guest_count", 0)
	if guests < 0 {
		guests = 0
	}
	outcome, err := s.store.AddParticipant(ctx, sess.ID, playerID, guests)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if err := s.sendConfirmation(ctx, *sess, outcome.Entry); err != nil {
		s.serverError(w, r, err)
		return
	}
	if outcome.Waitlisted {
		s.ok(w, r, outcome.Entry.PlayerName+" was added to the waitlist and emailed their link.", to)
		return
	}
	s.ok(w, r, outcome.Entry.PlayerName+" was added and emailed their link.", to)
}

func (s *Server) handleRemoveParticipant(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	rsvpID, err := strconv.ParseInt(r.PathValue("rsvpID"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	entry, err := s.store.Cancel(r.Context(), rsvpID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	name := "That person"
	if entry != nil {
		name = entry.PlayerName
	}
	s.ok(w, r, name+" was removed. Their spot is free again.", fmt.Sprintf("/sessions/%d", sess.ID))
}

// sendConfirmation queues the "you're in" (or "you're waitlisted") email that
// carries the participant's permanent manage link.
func (s *Server) sendConfirmation(ctx context.Context, sess model.Session, entry model.Entry) error {
	roster, err := s.store.Roster(ctx, sess)
	if err != nil {
		return err
	}
	mc, err := s.mailContext(ctx, sess, roster)
	if err != nil {
		return err
	}
	player, err := s.store.Player(ctx, entry.PlayerID)
	if err != nil || player == nil {
		return err
	}
	msg, err := mc.Confirmation(*player, entry)
	if err != nil {
		return err
	}
	if err := s.store.Enqueue(ctx, msg); err != nil {
		return err
	}
	s.worker.Wake()
	return nil
}

// notifyOrganizer tells the organizer someone dropped out, which is what
// prompts them to open the dashboard and promote from the waitlist.
func (s *Server) notifyOrganizer(ctx context.Context, sess model.Session, who string, guests int) error {
	roster, err := s.store.Roster(ctx, sess)
	if err != nil {
		return err
	}
	mc, err := s.mailContext(ctx, sess, roster)
	if err != nil {
		return err
	}
	msg, err := mc.OrganizerNotice(s.organizerAsPlayer(), who, guests)
	if err != nil {
		return err
	}
	// The organizer is not a mailing-list player, so this message has no
	// player id and no unsubscribe link.
	msg.PlayerID = nil
	msg.UnsubscribeURL = ""
	if err := s.store.Enqueue(ctx, msg); err != nil {
		return err
	}
	s.worker.Wake()
	return nil
}

func tokensByPlayer(entries []model.Entry) map[int64]string {
	m := make(map[int64]string, len(entries))
	for _, e := range entries {
		m[e.PlayerID] = e.Token
	}
	return m
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var _ = mail.ErrNotConfigured
