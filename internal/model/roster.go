package model

import (
	"fmt"
	"sort"
	"time"
)

// Roster is the computed view of one session's RSVPs. Every capacity and cost
// number in the app comes from here, so there is exactly one definition of
// "how many people are coming".
type Roster struct {
	Session   Session
	Confirmed []Entry
	Waitlist  []Entry // ordered by waitlist position
	Cancelled []Entry
	Invited   []Entry // invited but not yet answered
	Headcount int     // bodies, including guests
	SpotsLeft int
}

// BuildRoster splits entries by status and computes headcount. Headcount counts
// bodies rather than rows: a player with one guest is two.
func BuildRoster(s Session, entries []Entry) Roster {
	r := Roster{Session: s}
	for _, e := range entries {
		switch e.Status {
		case StatusConfirmed:
			r.Confirmed = append(r.Confirmed, e)
			r.Headcount += e.PartySize()
		case StatusWaitlist:
			r.Waitlist = append(r.Waitlist, e)
		case StatusCancelled:
			r.Cancelled = append(r.Cancelled, e)
		default:
			r.Invited = append(r.Invited, e)
		}
	}
	sort.SliceStable(r.Confirmed, func(i, j int) bool {
		return r.Confirmed[i].CreatedAt < r.Confirmed[j].CreatedAt
	})
	sort.SliceStable(r.Waitlist, func(i, j int) bool {
		return waitPos(r.Waitlist[i]) < waitPos(r.Waitlist[j])
	})
	r.SpotsLeft = s.MaxPlayers - r.Headcount
	if r.SpotsLeft < 0 {
		r.SpotsLeft = 0
	}
	return r
}

func waitPos(e Entry) int {
	if e.WaitlistPosition == nil {
		return 1 << 30
	}
	return *e.WaitlistPosition
}

// Fits reports whether a party of the given size can be confirmed right now.
// Signups are all-or-nothing: a person plus a guest takes two slots or goes on
// the waitlist as a pair, so nobody ends up half-admitted.
func (r Roster) Fits(partySize int) bool { return partySize <= r.SpotsLeft }

// IsFull reports whether there is no room left at all.
func (r Roster) IsFull() bool { return r.SpotsLeft <= 0 }

// NextWaitlistPosition is the position to assign to the next person joining the
// waitlist.
func (r Roster) NextWaitlistPosition() int {
	max := 0
	for _, e := range r.Waitlist {
		if p := waitPos(e); p < 1<<30 && p > max {
			max = p
		}
	}
	return max + 1
}

// NextPromotable returns the first waitlist entry whose whole party fits in the
// remaining spots, along with any entries ahead of it that were too big.
// Returning the skipped list lets the UI say why the queue was not followed in
// order, rather than silently reordering people.
func (r Roster) NextPromotable() (promote *Entry, skipped []Entry) {
	for i := range r.Waitlist {
		e := r.Waitlist[i]
		if r.Fits(e.PartySize()) {
			return &r.Waitlist[i], skipped
		}
		skipped = append(skipped, e)
	}
	return nil, skipped
}

// Cost is the money breakdown for a session at a given headcount.
type Cost struct {
	Minutes        int
	TotalCents     int64
	Headcount      int
	PerPlayerCents int64
	HasRounding    bool // per-player total exceeds the true cost because of rounding up
}

// Cost computes the court cost and the per-player split. Guests are included in
// the headcount: they occupy a spot, so they pay for one. The per-player amount
// is rounded up to the cent so the organizer is never short at the desk.
func (s Session) Cost(headcount int) Cost {
	minutes := s.Minutes()
	c := Cost{Minutes: minutes, Headcount: headcount}
	if minutes <= 0 || s.Courts <= 0 || s.CostPerCourtHourCents <= 0 {
		return c
	}
	// Integer math throughout: courts x rate x minutes / 60, rounded to nearest cent.
	numerator := int64(s.Courts) * s.CostPerCourtHourCents * int64(minutes)
	c.TotalCents = (numerator + 30) / 60
	if headcount > 0 {
		c.PerPlayerCents = (c.TotalCents + int64(headcount) - 1) / int64(headcount)
		c.HasRounding = c.PerPlayerCents*int64(headcount) != c.TotalCents
	}
	return c
}

// Display status values. These extend the stored set with the two derived ones.
const (
	DisplayDraft     = "Draft"
	DisplayOpen      = "Open"
	DisplayFull      = "Full"
	DisplayClosed    = "Closed"
	DisplayCompleted = "Completed"
	DisplayCancelled = "Cancelled"
)

// DisplayStatus derives the status shown in the UI. Cancelled wins over
// everything; a session in the past is completed; an open session past its
// deadline is closed without any job having to run.
func (r Roster) DisplayStatus(now time.Time, loc *time.Location) string {
	s := r.Session
	if s.Status == SessionCancelled {
		return DisplayCancelled
	}
	if now.After(s.EndAt(loc)) {
		return DisplayCompleted
	}
	switch s.Status {
	case SessionDraft:
		return DisplayDraft
	case SessionClosed:
		return DisplayClosed
	}
	if now.After(s.SignupDeadline()) {
		return DisplayClosed
	}
	if r.IsFull() {
		return DisplayFull
	}
	return DisplayOpen
}

// SignupsOpen reports whether the app will accept a new or changed RSVP. A full
// session still accepts signups: they go to the waitlist.
func (r Roster) SignupsOpen(now time.Time, loc *time.Location) bool {
	s := r.Session
	if s.Status != SessionOpen {
		return false
	}
	if now.After(s.SignupDeadline()) {
		return false
	}
	return now.Before(s.StartAt(loc))
}

// RateLine states the court rate and how it is shared, for the audiences who
// should not be given a figure that moves.
//
// Neither the total nor the per-player share is quoted: both change as people
// sign up, so a number shown to someone deciding whether to play is wrong by
// the time they play. An empty roster would advertise a share several times
// the real one. The rate is the only fixed part, and dividing it by the number
// of players is the rule everyone already understands.
func (s Session) RateLine() string {
	return FormatRate(s.CostPerCourtHourCents) + " per court per hour, divided by the number of players"
}

// FormatRate drops the trailing zeroes on a whole-dollar amount, so a rate
// reads as "$35" rather than "$35.00" in the middle of a sentence.
func FormatRate(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("$%d", cents/100)
	}
	return FormatCents(cents)
}

// FormatCents renders an amount as a dollar string.
func FormatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
