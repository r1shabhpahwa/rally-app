package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"badminton/internal/model"
	"badminton/internal/store"
)

// handleRSVP serves a participant's personal link. The same URL is the signup
// page before they answer and the manage page afterwards, which is why the
// token can be reused in every email we ever send them about this session.
func (s *Server) handleRSVP(w http.ResponseWriter, r *http.Request) {
	entry, sess, err := s.store.EntryByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if entry == nil || sess == nil {
		s.notFound(w, r)
		return
	}
	s.renderParticipant(w, r, *entry, *sess, "")
}

func (s *Server) renderParticipant(w http.ResponseWriter, r *http.Request, entry model.Entry, sess model.Session, _ string) {
	ctx := r.Context()
	roster, err := s.store.Roster(ctx, sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defaults, err := s.store.Defaults(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := s.now()

	// The spots figure shown to someone deciding whether to sign up must not
	// count the slots they are already holding, or an existing attendee sees
	// the session as fuller than it is for them.
	available := roster.SpotsLeft
	if entry.Status == model.StatusConfirmed {
		available += entry.PartySize()
	}

	responded := entry.Status == model.StatusConfirmed || entry.Status == model.StatusWaitlist

	s.renderPublic(w, r, "participant.html", view{
		Title: "Badminton RSVP",
		Data: map[string]any{
			"Entry":         entry,
			"Session":       sess,
			"Roster":        roster,
			"Status":        roster.DisplayStatus(now, s.cfg.Timezone),
			"Cost":          sess.Cost(roster.Headcount),
			"SignupsOpen":   roster.SignupsOpen(now, s.cfg.Timezone),
			"Responded":     responded,
			"Available":     available,
			"WouldWaitlist": available < 1,
			"ThingsToBring": defaults.ThingsToBring,
			"Deadline":      sess.SignupDeadline().In(s.cfg.Timezone).Format("Monday, January 2 at 3:04 PM"),
			"DateLine":      sess.StartAt(s.cfg.Timezone).Format("Monday, January 2"),
			"TimeLine": sess.StartAt(s.cfg.Timezone).Format("3:04 PM") + " - " +
				sess.EndAt(s.cfg.Timezone).Format("3:04 PM"),
		},
	})
}

func (s *Server) rsvpFromToken(w http.ResponseWriter, r *http.Request) (*model.Entry, *model.Session, bool) {
	entry, sess, err := s.store.EntryByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.serverError(w, r, err)
		return nil, nil, false
	}
	if entry == nil || sess == nil {
		s.notFound(w, r)
		return nil, nil, false
	}
	return entry, sess, true
}

func (s *Server) handleRSVPSignup(w http.ResponseWriter, r *http.Request) {
	entry, sess, ok := s.rsvpFromToken(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "That form could not be read.", "/r/"+entry.Token)
		return
	}
	back := "/r/" + entry.Token

	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		s.fail(w, r, "Please enter your name.", back)
		return
	}
	guests := formInt(r, "guest_count", 0)
	if guests < 0 || guests > 4 {
		s.fail(w, r, "You can bring between 0 and 4 guests.", back)
		return
	}

	outcome, err := s.store.Signup(r.Context(), entry.ID, name, guests, s.now(), s.cfg.Timezone, true)
	if err != nil {
		if errors.Is(err, store.ErrSignupsClosed) {
			s.fail(w, r, "Signups are closed for this session.", back)
			return
		}
		s.serverError(w, r, err)
		return
	}

	if err := s.sendConfirmation(r.Context(), *sess, outcome.Entry); err != nil {
		s.serverError(w, r, err)
		return
	}
	if outcome.Waitlisted {
		s.ok(w, r, fmt.Sprintf("You are on the waitlist at number %d. We have emailed you the details.", outcome.Position), back)
		return
	}
	s.ok(w, r, "You are confirmed. We have emailed you the details.", back)
}

func (s *Server) handleRSVPGuest(w http.ResponseWriter, r *http.Request) {
	entry, _, ok := s.rsvpFromToken(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "That form could not be read.", "/r/"+entry.Token)
		return
	}
	back := "/r/" + entry.Token

	guests := formInt(r, "guest_count", entry.GuestCount)
	if guests < 0 || guests > 4 {
		s.fail(w, r, "You can bring between 0 and 4 guests.", back)
		return
	}

	outcome, err := s.store.SetGuestCount(r.Context(), entry.ID, guests, s.now(), s.cfg.Timezone)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNoRoom):
			s.fail(w, r, "There is not enough room left for another guest. You can join the waitlist by cancelling and signing up again as a pair.", back)
		case errors.Is(err, store.ErrSignupsClosed):
			s.fail(w, r, "Signups are closed for this session, so this cannot be changed here. Message the organizer instead.", back)
		default:
			s.serverError(w, r, err)
		}
		return
	}
	if outcome.Entry.GuestCount == 0 {
		s.ok(w, r, "Your guest has been removed.", back)
		return
	}
	s.ok(w, r, fmt.Sprintf("Updated: you are bringing %d %s.",
		outcome.Entry.GuestCount, plural(outcome.Entry.GuestCount, "guest", "guests")), back)
}

func (s *Server) handleRSVPCancel(w http.ResponseWriter, r *http.Request) {
	entry, sess, ok := s.rsvpFromToken(w, r)
	if !ok {
		return
	}
	back := "/r/" + entry.Token
	wasAttending := entry.Status == model.StatusConfirmed

	updated, err := s.store.Cancel(r.Context(), entry.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	// Only bother the organizer when a confirmed spot actually opened up.
	if wasAttending {
		if err := s.notifyOrganizer(r.Context(), *sess, entry.PlayerName, entry.GuestCount); err != nil {
			s.log.Error("organizer notice failed", "err", err)
		}
	}
	_ = updated
	s.ok(w, r, "Your RSVP has been cancelled. You can sign up again from this page if your plans change.", back)
}

// handlePublicSignup is the fallback for a forwarded invitation or someone new,
// where we have no personal token to work from.
func (s *Server) handlePublicSignup(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.publicSession(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	roster, err := s.store.Roster(ctx, *sess)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defaults, err := s.store.Defaults(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := s.now()
	s.renderPublic(w, r, "public_signup.html", view{
		Title: "Badminton signup",
		Data: map[string]any{
			"Session":       *sess,
			"Roster":        roster,
			"Status":        roster.DisplayStatus(now, s.cfg.Timezone),
			"SignupsOpen":   roster.SignupsOpen(now, s.cfg.Timezone),
			"ThingsToBring": defaults.ThingsToBring,
			"Deadline":      sess.SignupDeadline().In(s.cfg.Timezone).Format("Monday, January 2 at 3:04 PM"),
			"DateLine":      sess.StartAt(s.cfg.Timezone).Format("Monday, January 2"),
			"TimeLine": sess.StartAt(s.cfg.Timezone).Format("3:04 PM") + " - " +
				sess.EndAt(s.cfg.Timezone).Format("3:04 PM"),
		},
	})
}

func (s *Server) handlePublicSignupPost(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.publicSession(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "That form could not be read.", "/s/"+sess.PublicID)
		return
	}
	ctx := r.Context()
	back := "/s/" + sess.PublicID

	name := strings.TrimSpace(r.PostFormValue("name"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	if name == "" || !looksLikeEmail(email) {
		s.fail(w, r, "Please enter your name and a valid email address.", back)
		return
	}
	guests := formInt(r, "guest_count", 0)
	if guests < 0 || guests > 4 {
		s.fail(w, r, "You can bring between 0 and 4 guests.", back)
		return
	}

	player, err := s.store.PlayerByEmail(ctx, email)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if player == nil {
		// Created inactive: signing up for one session is not the same as
		// asking to join the weekly mailing list. The organizer decides.
		player, err = s.store.CreatePlayer(ctx, name, email, false)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}

	entry, err := s.store.EnsureEntry(ctx, sess.ID, player.ID, model.SourcePublic)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	outcome, err := s.store.Signup(ctx, entry.ID, name, guests, s.now(), s.cfg.Timezone, true)
	if err != nil {
		if errors.Is(err, store.ErrSignupsClosed) {
			s.fail(w, r, "Signups are closed for this session.", back)
			return
		}
		s.serverError(w, r, err)
		return
	}
	if err := s.sendConfirmation(ctx, *sess, outcome.Entry); err != nil {
		s.serverError(w, r, err)
		return
	}

	// Send them on to their own permanent link, which is now their RSVP page.
	if outcome.Waitlisted {
		s.ok(w, r, fmt.Sprintf("You are on the waitlist at number %d. Bookmark this page to manage your RSVP.", outcome.Position),
			"/r/"+outcome.Entry.Token)
		return
	}
	s.ok(w, r, "You are confirmed. Bookmark this page to manage your RSVP.", "/r/"+outcome.Entry.Token)
}

func (s *Server) publicSession(w http.ResponseWriter, r *http.Request) (*model.Session, bool) {
	sess, err := s.store.SessionByPublicID(r.Context(), r.PathValue("publicID"))
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

func (s *Server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	player, err := s.store.PlayerByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if player == nil {
		s.notFound(w, r)
		return
	}
	s.renderPublic(w, r, "unsubscribe.html", view{
		Title: "Email preferences",
		Data:  map[string]any{"Player": *player, "Token": player.Token},
	})
}

func (s *Server) handleUnsubscribePost(w http.ResponseWriter, r *http.Request) {
	player, err := s.store.PlayerByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if player == nil {
		s.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, "That form could not be read.", "/u/"+player.Token)
		return
	}
	resubscribe := r.PostFormValue("resubscribe") != ""
	if err := s.store.SetUnsubscribed(r.Context(), player.ID, !resubscribe); err != nil {
		s.serverError(w, r, err)
		return
	}
	if resubscribe {
		s.ok(w, r, "You are back on the list and will get the weekly email again.", "/u/"+player.Token)
		return
	}
	s.ok(w, r, "You have been unsubscribed and will not get any more badminton emails.", "/u/"+player.Token)
}
