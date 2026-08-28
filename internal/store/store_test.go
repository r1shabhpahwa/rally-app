package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"badminton/internal/model"
)

var ctx = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// fixture builds a session with maxPlayers capacity and n invited players.
func fixture(t *testing.T, s *Store, maxPlayers, n int) (model.Session, []model.Entry) {
	t.Helper()
	sess, err := s.CreateSession(ctx, model.Session{
		Date: "2026-09-01", StartTime: "19:00", EndTime: "21:00",
		Courts: 3, CostPerCourtHourCents: 3500, MaxPlayers: maxPlayers,
		SignupDeadlineAt: mkTime(t, "2026-08-29 15:00").Unix(),
		Status:           model.SessionOpen,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var players []model.Player
	for i := 0; i < n; i++ {
		p, err := s.CreatePlayer(ctx, string(rune('A'+i)), string(rune('a'+i))+"@example.com", true)
		if err != nil {
			t.Fatalf("create player: %v", err)
		}
		players = append(players, *p)
	}
	entries, err := s.EnsureInvites(ctx, sess.ID, players)
	if err != nil {
		t.Fatalf("ensure invites: %v", err)
	}
	return *sess, entries
}

func rosterOf(t *testing.T, s *Store, sess model.Session) model.Roster {
	t.Helper()
	r, err := s.Roster(ctx, sess)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	return r
}

func TestEnsureInvitesIsIdempotentAndTokensAreUnique(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 12, 3)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Status != model.StatusInvited {
			t.Fatalf("entry status = %s, want invited", e.Status)
		}
		if e.Token == "" || seen[e.Token] {
			t.Fatalf("token %q is empty or duplicated", e.Token)
		}
		seen[e.Token] = true
	}

	// Calling again (as a reminder top-up does) must not duplicate rows.
	players, _ := s.MailablePlayers(ctx)
	again, err := s.EnsureInvites(ctx, sess.ID, players)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(again) != 3 {
		t.Fatalf("got %d entries after re-invite, want 3", len(again))
	}
}

func TestReminderTopsUpNewMailingListMembers(t *testing.T) {
	s := testStore(t)
	sess, _ := fixture(t, s, 12, 2)

	// Someone joins the mailing list after the invitation went out.
	if _, err := s.CreatePlayer(ctx, "Late", "late@example.com", true); err != nil {
		t.Fatal(err)
	}
	players, _ := s.MailablePlayers(ctx)
	entries, err := s.EnsureInvites(ctx, sess.ID, players)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want the late joiner to have been given a token", len(entries))
	}
}

func TestSignupFillsThenWaitlists(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 2, 3)
	now := mkTime(t, "2026-08-28 10:00")

	for i := 0; i < 2; i++ {
		out, err := s.Signup(ctx, entries[i].ID, "", 0, now, time.UTC, true)
		if err != nil {
			t.Fatalf("signup %d: %v", i, err)
		}
		if out.Waitlisted {
			t.Fatalf("signup %d was waitlisted, want confirmed", i)
		}
	}

	out, err := s.Signup(ctx, entries[2].ID, "", 0, now, time.UTC, true)
	if err != nil {
		t.Fatalf("third signup: %v", err)
	}
	if !out.Waitlisted || out.Position != 1 {
		t.Fatalf("third signup = %+v, want waitlisted at position 1", out)
	}

	r := rosterOf(t, s, sess)
	if r.Headcount != 2 || len(r.Waitlist) != 1 {
		t.Fatalf("roster = %d confirmed bodies, %d waitlisted; want 2 and 1", r.Headcount, len(r.Waitlist))
	}
}

func TestGuestPartyIsAllOrNothing(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 3, 2)
	now := mkTime(t, "2026-08-28 10:00")

	// Two confirmed leaves one spot.
	if _, err := s.Signup(ctx, entries[0].ID, "", 1, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	if r := rosterOf(t, s, sess); r.Headcount != 2 {
		t.Fatalf("headcount = %d, want 2 (player + guest)", r.Headcount)
	}

	// A party of 2 must not squeeze into the single remaining spot.
	out, err := s.Signup(ctx, entries[1].ID, "", 1, now, time.UTC, true)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Waitlisted {
		t.Fatal("a party of 2 was confirmed into 1 remaining spot")
	}
	if r := rosterOf(t, s, sess); r.Headcount != 2 {
		t.Fatalf("headcount = %d after waitlisting, want it unchanged at 2", r.Headcount)
	}
}

func TestAddGuestRefusedWhenNoRoom(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 2, 2)
	now := mkTime(t, "2026-08-28 10:00")
	if _, err := s.Signup(ctx, entries[0].ID, "", 0, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Signup(ctx, entries[1].ID, "", 0, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	_, err := s.SetGuestCount(ctx, entries[0].ID, 1, now, time.UTC)
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("adding a guest to a full session returned %v, want ErrNoRoom", err)
	}
	if r := rosterOf(t, s, sess); r.Headcount != 2 {
		t.Fatalf("headcount = %d, want it unchanged at 2", r.Headcount)
	}
}

func TestAddGuestAllowedWhenRoomExists(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 4, 1)
	now := mkTime(t, "2026-08-28 10:00")
	if _, err := s.Signup(ctx, entries[0].ID, "", 0, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGuestCount(ctx, entries[0].ID, 1, now, time.UTC); err != nil {
		t.Fatalf("add guest: %v", err)
	}
	if r := rosterOf(t, s, sess); r.Headcount != 2 {
		t.Fatalf("headcount = %d, want 2", r.Headcount)
	}
}

func TestCancelFreesSpotsAndPromotionFillsThem(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 2, 3)
	now := mkTime(t, "2026-08-28 10:00")
	for i := 0; i < 3; i++ {
		if _, err := s.Signup(ctx, entries[i].ID, "", 0, now, time.UTC, true); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.Cancel(ctx, entries[0].ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r := rosterOf(t, s, sess)
	if r.SpotsLeft != 1 {
		t.Fatalf("spotsLeft after cancel = %d, want 1", r.SpotsLeft)
	}
	// Cancellation must not auto-promote: the organizer decides.
	if len(r.Waitlist) != 1 {
		t.Fatalf("waitlist = %d, want the waitlisted person to still be waiting", len(r.Waitlist))
	}

	promoted, skipped, err := s.PromoteNext(ctx, sess.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted == nil || promoted.ID != entries[2].ID {
		t.Fatalf("promoted = %+v, want the waitlisted entry", promoted)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want none", skipped)
	}
	if promoted.Status != model.StatusConfirmed {
		t.Fatalf("promoted status = %s, want confirmed (no claim step)", promoted.Status)
	}
	if r := rosterOf(t, s, sess); r.Headcount != 2 || len(r.Waitlist) != 0 {
		t.Fatalf("after promotion: headcount %d, waitlist %d; want 2 and 0", r.Headcount, len(r.Waitlist))
	}
}

func TestPromoteSkipsPartyThatDoesNotFitAndReportsIt(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 3, 4)
	now := mkTime(t, "2026-08-28 10:00")

	// Fill 3 of 3, then waitlist a pair followed by a single.
	if _, err := s.Signup(ctx, entries[0].ID, "", 2, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Signup(ctx, entries[1].ID, "", 1, now, time.UTC, true); err != nil { // waitlist, needs 2
		t.Fatal(err)
	}
	if _, err := s.Signup(ctx, entries[2].ID, "", 0, now, time.UTC, true); err != nil { // waitlist, needs 1
		t.Fatal(err)
	}
	// Free exactly one spot by dropping a guest from the confirmed party.
	if _, err := s.SetGuestCount(ctx, entries[0].ID, 1, now, time.UTC); err != nil {
		t.Fatal(err)
	}

	promoted, skipped, err := s.PromoteNext(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted == nil || promoted.ID != entries[2].ID {
		t.Fatalf("promoted = %+v, want the single who fits", promoted)
	}
	if len(skipped) != 1 || skipped[0].ID != entries[1].ID {
		t.Fatalf("skipped = %+v, want the oversized party reported", skipped)
	}
}

func TestTokenLifecycle(t *testing.T) {
	s := testStore(t)
	sess, entries := fixture(t, s, 12, 1)
	now := mkTime(t, "2026-08-28 10:00")
	token := entries[0].Token

	e, loaded, err := s.EntryByToken(ctx, token)
	if err != nil || e == nil || loaded == nil {
		t.Fatalf("lookup by token: %v %v %v", err, e, loaded)
	}
	if loaded.ID != sess.ID || e.Status != model.StatusInvited {
		t.Fatalf("token resolved to the wrong row: %+v", e)
	}

	if _, err := s.Signup(ctx, e.ID, "Renamed Person", 0, now, time.UTC, true); err != nil {
		t.Fatal(err)
	}
	e, _, _ = s.EntryByToken(ctx, token)
	if e.Status != model.StatusConfirmed || e.PlayerName != "Renamed Person" {
		t.Fatalf("after signup: %+v, want confirmed with the corrected name", e)
	}

	if _, err := s.Cancel(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	e, _, _ = s.EntryByToken(ctx, token)
	if e.Status != model.StatusCancelled {
		t.Fatalf("after cancel: %s, want cancelled", e.Status)
	}

	// The same token must still work for signing up again.
	if _, err := s.Signup(ctx, e.ID, "", 0, now, time.UTC, true); err != nil {
		t.Fatalf("re-signup on the same token: %v", err)
	}
	e, _, _ = s.EntryByToken(ctx, token)
	if e.Status != model.StatusConfirmed {
		t.Fatalf("after re-signup: %s, want confirmed", e.Status)
	}
}

func TestSignupRejectedAfterDeadline(t *testing.T) {
	s := testStore(t)
	_, entries := fixture(t, s, 12, 1)
	late := mkTime(t, "2026-08-30 10:00")
	_, err := s.Signup(ctx, entries[0].ID, "", 0, late, time.UTC, true)
	if !errors.Is(err, ErrSignupsClosed) {
		t.Fatalf("late signup returned %v, want ErrSignupsClosed", err)
	}
}

func TestOrganizerCanAddAfterDeadline(t *testing.T) {
	s := testStore(t)
	sess, _ := fixture(t, s, 12, 0)
	p, err := s.CreatePlayer(ctx, "Texted Me", "texted@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.AddParticipant(ctx, sess.ID, p.ID, 0)
	if err != nil {
		t.Fatalf("manual add: %v", err)
	}
	if out.Waitlisted {
		t.Fatal("manual add was waitlisted despite free spots")
	}
	if r := rosterOf(t, s, sess); r.Headcount != 1 {
		t.Fatalf("headcount = %d, want 1", r.Headcount)
	}
}

func TestDuplicateEmailRejected(t *testing.T) {
	s := testStore(t)
	if _, err := s.CreatePlayer(ctx, "A", "dup@example.com", true); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive: the same person with different capitalisation.
	_, err := s.CreatePlayer(ctx, "B", "DUP@example.com", true)
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Fatalf("duplicate email returned %v, want ErrDuplicateEmail", err)
	}
}

func TestImportPlayersUpsertsByEmail(t *testing.T) {
	s := testStore(t)
	people := []struct{ Name, Email string }{
		{"Alice Smith", "alice@example.com"},
		{"David", "david@example.com"},
	}
	res, err := s.ImportPlayers(ctx, people)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 {
		t.Fatalf("added = %d, want 2", res.Added)
	}

	people[0].Name = "Alice S."
	res, err = s.ImportPlayers(ctx, people)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 2 || res.Added != 0 {
		t.Fatalf("re-import = %+v, want 2 updated and 0 added", res)
	}
	all, _ := s.Players(ctx, "")
	if len(all) != 2 {
		t.Fatalf("player count = %d, want 2 (no duplicates)", len(all))
	}
}

func TestUnsubscribeRemovesFromMailingList(t *testing.T) {
	s := testStore(t)
	p, _ := s.CreatePlayer(ctx, "A", "a@example.com", true)
	if err := s.SetUnsubscribed(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	mail, _ := s.MailablePlayers(ctx)
	if len(mail) != 0 {
		t.Fatalf("mailable = %d, want 0 after unsubscribe", len(mail))
	}
}

func TestOutboxRetryThenSend(t *testing.T) {
	s := testStore(t)
	if err := s.Enqueue(ctx, model.OutboxMessage{
		Kind: model.KindTest, ToEmail: "a@example.com", Subject: "hi", TextBody: "x",
	}); err != nil {
		t.Fatal(err)
	}
	due, err := s.ClaimDue(ctx, time.Now().Unix(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim = %d, %v; want 1 message", len(due), err)
	}

	future := time.Now().Add(time.Hour).Unix()
	if err := s.MarkRetry(ctx, due[0].ID, future, "smtp down"); err != nil {
		t.Fatal(err)
	}
	later, _ := s.ClaimDue(ctx, time.Now().Unix(), 10)
	if len(later) != 0 {
		t.Fatal("a message scheduled for the future was claimed early")
	}

	if err := s.MarkSent(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	n, _ := s.PendingCount(ctx)
	if n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "twice.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an existing database re-ran migrations: %v", err)
	}
	s2.Close()
}
