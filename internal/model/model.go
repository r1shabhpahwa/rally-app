// Package model holds the domain types and the rules that govern them:
// capacity, waitlisting, cost splitting and status derivation.
package model

import "time"

// RSVP status values. A row is created in StatusInvited when the invitation is
// sent, which is what lets one token serve as both the signup link and the
// permanent "manage my RSVP" link.
const (
	StatusInvited   = "invited"
	StatusConfirmed = "confirmed"
	StatusWaitlist  = "waitlist"
	StatusCancelled = "cancelled"
)

// Stored session status values. "full" and "completed" are deliberately absent:
// they are derived, because a stored copy drifts the moment someone cancels.
const (
	SessionDraft     = "draft"
	SessionOpen      = "open"
	SessionClosed    = "closed"
	SessionCancelled = "cancelled"
)

// Where an RSVP came from, so the dashboard can flag people who are not on the
// mailing list yet.
const (
	SourceInvite = "invite"
	SourcePublic = "public"
	SourceManual = "manual"
)

// Player is someone who can be invited. The mailing list is every player with
// Active set and no unsubscribe.
type Player struct {
	ID             int64
	Name           string
	Email          string
	Active         bool
	Token          string
	UnsubscribedAt *int64
	CreatedAt      int64
	UpdatedAt      int64
}

// Mailable reports whether this player should receive group email.
func (p Player) Mailable() bool { return p.Active && p.UnsubscribedAt == nil }

// Session is one badminton evening.
type Session struct {
	ID                    int64
	PublicID              string
	Date                  string // YYYY-MM-DD, local
	StartTime             string // HH:MM, local
	EndTime               string // HH:MM, local
	Courts                int
	CostPerCourtHourCents int64
	MaxPlayers            int
	SignupDeadlineAt      int64
	Status                string
	Notes                 string
	InvitationSentAt      *int64
	LastReminderAt        *int64
	CreatedAt             int64
	UpdatedAt             int64
}

// RSVP is one person's answer for one session.
type RSVP struct {
	ID               int64
	SessionID        int64
	PlayerID         int64
	Status           string
	GuestCount       int
	Token            string
	WaitlistPosition *int
	Source           string
	CreatedAt        int64
	UpdatedAt        int64
}

// PartySize counts the person plus any guests. Capacity and cost are both
// computed in party sizes, never in rows.
func (r RSVP) PartySize() int { return 1 + r.GuestCount }

// Entry pairs an RSVP with the player it belongs to, for roster display.
type Entry struct {
	RSVP
	PlayerName   string
	PlayerEmail  string
	PlayerActive bool
}

// CourtDeadline is the last moment a given court can be released without charge.
type CourtDeadline struct {
	ID          int64
	SessionID   int64
	CourtNumber int
	DeadlineAt  int64
}

// OutboxMessage is one queued email.
type OutboxMessage struct {
	ID             int64
	Kind           string
	SessionID      *int64
	PlayerID       *int64
	ToEmail        string
	ToName         string
	Subject        string
	TextBody       string
	HTMLBody       string
	UnsubscribeURL string
	Status         string
	Attempts       int
	NextAttemptAt  int64
	LastError      string
	CreatedAt      int64
	SentAt         *int64
}

// Outbox message states.
const (
	OutboxPending = "pending"
	OutboxSent    = "sent"
	OutboxFailed  = "failed"
)

// Email kinds, used for the delivery log and for suppressing duplicates.
const (
	KindInvitation   = "invitation"
	KindConfirmation = "confirmation"
	KindReminder     = "reminder"
	KindPromoted     = "promoted"
	KindCancelled    = "session_cancelled"
	KindOrganizer    = "organizer_notice"
	KindTest         = "test"
)

// StartAt returns the session's start as an instant in loc.
func (s Session) StartAt(loc *time.Location) time.Time {
	return parseLocal(s.Date, s.StartTime, loc)
}

// EndAt returns the session's end as an instant in loc, rolling to the next day
// if the end time is earlier than the start time.
func (s Session) EndAt(loc *time.Location) time.Time {
	start := s.StartAt(loc)
	end := parseLocal(s.Date, s.EndTime, loc)
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	return end
}

// Minutes is the session's booked length in minutes.
func (s Session) Minutes() int {
	loc := time.UTC
	return int(s.EndAt(loc).Sub(s.StartAt(loc)) / time.Minute)
}

// SignupDeadline returns the deadline as a time.Time.
func (s Session) SignupDeadline() time.Time { return time.Unix(s.SignupDeadlineAt, 0) }

func parseLocal(date, clock string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}
