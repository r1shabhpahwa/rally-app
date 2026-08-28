package model

import (
	"testing"
	"time"
)

var loc = time.UTC

func sess() Session {
	return Session{
		Date: "2026-09-01", StartTime: "19:00", EndTime: "21:00",
		Courts: 3, CostPerCourtHourCents: 3500, MaxPlayers: 12,
		SignupDeadlineAt: mustTime("2026-08-29 15:00").Unix(),
		Status:           SessionOpen,
	}
}

func mustTime(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func entry(id int64, status string, guests int, pos int, created int64) Entry {
	e := Entry{RSVP: RSVP{ID: id, Status: status, GuestCount: guests, CreatedAt: created}}
	if status == StatusWaitlist {
		p := pos
		e.WaitlistPosition = &p
	}
	return e
}

func TestHeadcountIncludesGuests(t *testing.T) {
	s := sess()
	r := BuildRoster(s, []Entry{
		entry(1, StatusConfirmed, 0, 0, 1),
		entry(2, StatusConfirmed, 1, 0, 2), // person + guest = 2 bodies
		entry(3, StatusWaitlist, 0, 1, 3),
		entry(4, StatusCancelled, 0, 0, 4),
	})
	if r.Headcount != 3 {
		t.Fatalf("headcount = %d, want 3 (guests count as bodies)", r.Headcount)
	}
	if r.SpotsLeft != 9 {
		t.Fatalf("spotsLeft = %d, want 9", r.SpotsLeft)
	}
	if len(r.Waitlist) != 1 || len(r.Cancelled) != 1 {
		t.Fatalf("statuses not split correctly: %+v", r)
	}
}

func TestPartyOfTwoIsAllOrNothing(t *testing.T) {
	s := sess()
	s.MaxPlayers = 3

	// 2 confirmed, 1 spot free: a party of 2 must not squeeze in.
	r := BuildRoster(s, []Entry{
		entry(1, StatusConfirmed, 0, 0, 1),
		entry(2, StatusConfirmed, 0, 0, 2),
	})
	if r.SpotsLeft != 1 {
		t.Fatalf("spotsLeft = %d, want 1", r.SpotsLeft)
	}
	if r.Fits(2) {
		t.Fatal("a party of 2 must not fit into 1 remaining spot")
	}
	if !r.Fits(1) {
		t.Fatal("a party of 1 should fit into 1 remaining spot")
	}

	// 1 confirmed, 2 spots free: the same party of 2 fits.
	r2 := BuildRoster(s, []Entry{entry(1, StatusConfirmed, 0, 0, 1)})
	if !r2.Fits(2) {
		t.Fatal("a party of 2 should fit into 2 remaining spots")
	}
}

func TestNextPromotableSkipsPartiesThatDoNotFit(t *testing.T) {
	s := sess()
	s.MaxPlayers = 3
	// 2 confirmed => 1 spot. Waitlist head is a party of 2, next is a single.
	r := BuildRoster(s, []Entry{
		entry(1, StatusConfirmed, 0, 0, 1),
		entry(2, StatusConfirmed, 0, 0, 2),
		entry(10, StatusWaitlist, 1, 1, 3), // needs 2 spots
		entry(11, StatusWaitlist, 0, 2, 4), // needs 1
	})
	promote, skipped := r.NextPromotable()
	if promote == nil || promote.ID != 11 {
		t.Fatalf("promote = %+v, want the single at ID 11", promote)
	}
	if len(skipped) != 1 || skipped[0].ID != 10 {
		t.Fatalf("skipped = %+v, want the oversized party at ID 10 reported", skipped)
	}
}

func TestNextPromotableNoneWhenFull(t *testing.T) {
	s := sess()
	s.MaxPlayers = 2
	r := BuildRoster(s, []Entry{
		entry(1, StatusConfirmed, 1, 0, 1), // fills both spots
		entry(10, StatusWaitlist, 0, 1, 2),
	})
	promote, skipped := r.NextPromotable()
	if promote != nil {
		t.Fatalf("promote = %+v, want none when the session is full", promote)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", len(skipped))
	}
}

func TestWaitlistOrderedByPosition(t *testing.T) {
	r := BuildRoster(sess(), []Entry{
		entry(3, StatusWaitlist, 0, 3, 1),
		entry(1, StatusWaitlist, 0, 1, 2),
		entry(2, StatusWaitlist, 0, 2, 3),
	})
	got := []int64{r.Waitlist[0].ID, r.Waitlist[1].ID, r.Waitlist[2].ID}
	want := []int64{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waitlist order = %v, want %v", got, want)
		}
	}
	if r.NextWaitlistPosition() != 4 {
		t.Fatalf("next position = %d, want 4", r.NextWaitlistPosition())
	}
}

func TestCostSplit(t *testing.T) {
	s := sess() // 3 courts x 2h x $35 = $210
	c := s.Cost(9)
	if c.TotalCents != 21000 {
		t.Fatalf("total = %d, want 21000", c.TotalCents)
	}
	// 21000 / 9 = 2333.33 -> rounded up to 2334 so the organizer is never short.
	if c.PerPlayerCents != 2334 {
		t.Fatalf("perPlayer = %d, want 2334", c.PerPlayerCents)
	}
	if !c.HasRounding {
		t.Fatal("expected rounding to be flagged")
	}
	if got := FormatCents(c.PerPlayerCents); got != "$23.34" {
		t.Fatalf("formatted = %s, want $23.34", got)
	}
}

func TestCostSplitIncludesGuests(t *testing.T) {
	s := sess()
	r := BuildRoster(s, []Entry{
		entry(1, StatusConfirmed, 0, 0, 1),
		entry(2, StatusConfirmed, 1, 0, 2),
	})
	c := s.Cost(r.Headcount)
	if c.Headcount != 3 {
		t.Fatalf("cost headcount = %d, want 3 (the guest pays too)", c.Headcount)
	}
	if c.PerPlayerCents != 7000 {
		t.Fatalf("perPlayer = %d, want 7000", c.PerPlayerCents)
	}
}

func TestCostZeroHeadcountDoesNotDivideByZero(t *testing.T) {
	c := sess().Cost(0)
	if c.TotalCents != 21000 || c.PerPlayerCents != 0 {
		t.Fatalf("got %+v, want total 21000 and no per-player split", c)
	}
}

func TestCostHalfHourSession(t *testing.T) {
	s := sess()
	s.EndTime = "20:30" // 1.5 hours
	s.Courts = 1
	c := s.Cost(2)
	if c.TotalCents != 5250 {
		t.Fatalf("total = %d, want 5250 for 1.5h at $35", c.TotalCents)
	}
}

func TestDisplayStatus(t *testing.T) {
	before := mustTime("2026-08-28 10:00")
	afterDeadline := mustTime("2026-08-30 10:00")
	afterSession := mustTime("2026-09-02 10:00")

	full := BuildRoster(func() Session { s := sess(); s.MaxPlayers = 1; return s }(),
		[]Entry{entry(1, StatusConfirmed, 0, 0, 1)})
	if got := full.DisplayStatus(before, loc); got != DisplayFull {
		t.Fatalf("full session = %s, want %s", got, DisplayFull)
	}

	open := BuildRoster(sess(), nil)
	if got := open.DisplayStatus(before, loc); got != DisplayOpen {
		t.Fatalf("open session = %s, want %s", got, DisplayOpen)
	}
	if got := open.DisplayStatus(afterDeadline, loc); got != DisplayClosed {
		t.Fatalf("past deadline = %s, want %s", got, DisplayClosed)
	}
	if got := open.DisplayStatus(afterSession, loc); got != DisplayCompleted {
		t.Fatalf("past session = %s, want %s", got, DisplayCompleted)
	}

	cancelled := BuildRoster(func() Session { s := sess(); s.Status = SessionCancelled; return s }(), nil)
	if got := cancelled.DisplayStatus(afterSession, loc); got != DisplayCancelled {
		t.Fatalf("cancelled = %s, want %s (cancelled beats completed)", got, DisplayCancelled)
	}

	draft := BuildRoster(func() Session { s := sess(); s.Status = SessionDraft; return s }(), nil)
	if got := draft.DisplayStatus(before, loc); got != DisplayDraft {
		t.Fatalf("draft = %s, want %s", got, DisplayDraft)
	}
}

func TestSignupsOpen(t *testing.T) {
	r := BuildRoster(sess(), nil)
	if !r.SignupsOpen(mustTime("2026-08-28 10:00"), loc) {
		t.Fatal("signups should be open before the deadline")
	}
	if r.SignupsOpen(mustTime("2026-08-29 15:01"), loc) {
		t.Fatal("signups should close once the deadline passes")
	}
	if r.SignupsOpen(mustTime("2026-09-01 20:00"), loc) {
		t.Fatal("signups should close once the session has started")
	}

	// A full session still accepts signups; they land on the waitlist.
	fullSess := sess()
	fullSess.MaxPlayers = 1
	full := BuildRoster(fullSess, []Entry{entry(1, StatusConfirmed, 0, 0, 1)})
	if !full.SignupsOpen(mustTime("2026-08-28 10:00"), loc) {
		t.Fatal("a full session must still accept signups so they can be waitlisted")
	}

	closed := BuildRoster(func() Session { s := sess(); s.Status = SessionClosed; return s }(), nil)
	if closed.SignupsOpen(mustTime("2026-08-28 10:00"), loc) {
		t.Fatal("a manually closed session must not accept signups")
	}
}

func TestSessionTimes(t *testing.T) {
	s := sess()
	if s.Minutes() != 120 {
		t.Fatalf("minutes = %d, want 120", s.Minutes())
	}
	// A session ending after midnight rolls to the next day rather than going negative.
	s.EndTime = "00:30"
	if s.Minutes() != 330 {
		t.Fatalf("overnight minutes = %d, want 330", s.Minutes())
	}
}
