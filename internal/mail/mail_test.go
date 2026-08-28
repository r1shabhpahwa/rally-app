package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"badminton/internal/model"
	"badminton/internal/store"
)

var ctx = context.Background()

// fakeSender fails the first failures attempts, then succeeds.
type fakeSender struct {
	failures  int
	calls     int
	permanent bool
	sent      []model.OutboxMessage
}

func (f *fakeSender) Send(_ context.Context, m model.OutboxMessage) error {
	f.calls++
	if f.permanent {
		return fmt.Errorf("%w: bad address", errPermanent)
	}
	if f.calls <= f.failures {
		return errors.New("connection refused")
	}
	f.sent = append(f.sent, m)
	return nil
}

func testWorker(t *testing.T, sender Sender) (*Worker, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := NewWorker(st, sender, log, 1000) // effectively no pacing delay in tests
	return w, st
}

func queue(t *testing.T, st *store.Store) model.OutboxMessage {
	t.Helper()
	if err := st.Enqueue(ctx, model.OutboxMessage{
		Kind: model.KindTest, ToEmail: "a@example.com", Subject: "hi", TextBody: "body",
	}); err != nil {
		t.Fatal(err)
	}
	due, err := st.ClaimDue(ctx, time.Now().Unix(), 1)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim: %v", err)
	}
	return due[0]
}

func TestWorkerDeliversAndMarksSent(t *testing.T) {
	sender := &fakeSender{}
	w, st := testWorker(t, sender)
	queue(t, st)

	w.drain(ctx)

	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	pending, _ := st.PendingCount(ctx)
	if pending != 0 {
		t.Fatalf("pending = %d, want 0", pending)
	}
}

func TestWorkerRetriesTransientFailureWithBackoff(t *testing.T) {
	sender := &fakeSender{failures: 1}
	w, st := testWorker(t, sender)
	msg := queue(t, st)

	w.deliver(ctx, msg)

	// Still pending, scheduled into the future, not failed.
	pending, _ := st.PendingCount(ctx)
	if pending != 1 {
		t.Fatalf("pending = %d, want the message left queued for a retry", pending)
	}
	if due, _ := st.ClaimDue(ctx, time.Now().Unix(), 10); len(due) != 0 {
		t.Fatal("a message awaiting backoff was claimable immediately")
	}
	if due, _ := st.ClaimDue(ctx, time.Now().Add(2*time.Minute).Unix(), 10); len(due) != 1 {
		t.Fatal("the message was not scheduled within the first backoff window")
	}
}

func TestWorkerGivesUpAfterAttemptCap(t *testing.T) {
	sender := &fakeSender{failures: 100}
	w, st := testWorker(t, sender)
	msg := queue(t, st)

	msg.Attempts = maxAttempts - 1 // the coming attempt is the last one
	w.deliver(ctx, msg)

	msgs, _ := st.RecentMessages(ctx, 10)
	if msgs[0].Status != model.OutboxFailed {
		t.Fatalf("status = %s, want failed after the attempt cap", msgs[0].Status)
	}
	if msgs[0].LastError == "" {
		t.Fatal("a failed message should record why, for the delivery log")
	}
}

func TestWorkerDoesNotRetryPermanentFailures(t *testing.T) {
	sender := &fakeSender{permanent: true}
	w, st := testWorker(t, sender)
	msg := queue(t, st)

	w.deliver(ctx, msg)

	msgs, _ := st.RecentMessages(ctx, 10)
	if msgs[0].Status != model.OutboxFailed {
		t.Fatalf("status = %s, want failed immediately for a permanent error", msgs[0].Status)
	}
	if msgs[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a permanent failure must not be retried", msgs[0].Attempts)
	}
}

func TestFailedMessageCanBeRequeued(t *testing.T) {
	sender := &fakeSender{failures: 1}
	w, st := testWorker(t, sender)
	msg := queue(t, st)
	msg.Attempts = maxAttempts - 1
	w.deliver(ctx, msg)

	if err := st.RetryMessage(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	w.drain(ctx)
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d, want the requeued message to go out", len(sender.sent))
	}
}

func TestDisabledSenderFailsLoudly(t *testing.T) {
	// Unconfigured SMTP must not look like a successful send.
	w, st := testWorker(t, DisabledSender{})
	queue(t, st)
	w.drain(ctx)

	msgs, _ := st.RecentMessages(ctx, 10)
	if msgs[0].Status != model.OutboxFailed {
		t.Fatalf("status = %s, want failed when SMTP is not configured", msgs[0].Status)
	}
	if !strings.Contains(msgs[0].LastError, "SMTP is not configured") {
		t.Fatalf("last error = %q, want it to name the missing configuration", msgs[0].LastError)
	}
}

// --- composition ---

func testContext() Context {
	loc := time.UTC
	sess := model.Session{
		ID: 1, PublicID: "pub123", Date: "2026-09-01", StartTime: "19:00", EndTime: "21:00",
		Courts: 3, CostPerCourtHourCents: 3500, MaxPlayers: 12,
		SignupDeadlineAt: time.Date(2026, 8, 29, 15, 0, 0, 0, loc).Unix(),
		Status:           model.SessionOpen,
	}
	roster := model.BuildRoster(sess, []model.Entry{
		{RSVP: model.RSVP{ID: 1, Status: model.StatusConfirmed}, PlayerName: "David"},
	})
	sess.Venue = "Bonsor Recreation Complex, 6550 Bonsor Ave, Burnaby"
	return Context{
		Session: sess, Roster: roster, Loc: loc, BaseURL: "https://badminton.test",
		ThingsToBring: "Racquet and shoes", OrganizerName: "Alice Smith",
		OrganizerEmail: "alice@example.com",
	}
}

func TestInvitationCarriesTheDetailsAndThePersonalLink(t *testing.T) {
	c := testContext()
	p := model.Player{ID: 7, Name: "Diana Prince", Email: "diana@example.com", Token: "unsub7"}

	msg, err := c.Invitation(p, "tok123")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Tuesday, September 1",            // date
		"7:00 PM - 9:00 PM",               // time
		"Saturday, August 29",             // deadline
		"$210.00",                         // cost
		"11 spots left",                   // remaining capacity
		"Racquet and shoes",               // things to bring
		"https://badminton.test/r/tok123", // their personal link
	} {
		if !strings.Contains(msg.TextBody, want) {
			t.Errorf("plain-text invitation is missing %q", want)
		}
		if !strings.Contains(msg.HTMLBody, want) {
			t.Errorf("HTML invitation is missing %q", want)
		}
	}
	if !strings.Contains(msg.TextBody, "Diana") {
		t.Error("the invitation does not greet the recipient by name")
	}
	if msg.UnsubscribeURL != "https://badminton.test/u/unsub7" {
		t.Errorf("unsubscribe url = %q", msg.UnsubscribeURL)
	}
	if msg.ToEmail != "diana@example.com" {
		t.Errorf("recipient = %q", msg.ToEmail)
	}
}

func TestWaitlistConfirmationSaysSoPlainly(t *testing.T) {
	c := testContext()
	p := model.Player{Name: "Sarah", Email: "sarah@example.com", Token: "u"}
	entry := model.Entry{RSVP: model.RSVP{Status: model.StatusWaitlist, Token: "tok"}, PlayerName: "Sarah"}

	msg, err := c.Confirmation(p, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Subject, "Waitlisted") {
		t.Errorf("subject = %q, want it to say waitlisted", msg.Subject)
	}
	if !strings.Contains(msg.TextBody, "waitlist") {
		t.Error("the body does not mention the waitlist")
	}
}

func TestPromotionEmailAsksForNothing(t *testing.T) {
	// Promotion confirms outright, so the email must not imply a spot to claim.
	c := testContext()
	p := model.Player{Name: "Diana", Email: "diana@example.com", Token: "u"}
	entry := model.Entry{RSVP: model.RSVP{Status: model.StatusConfirmed, Token: "tok"}, PlayerName: "Diana"}

	msg, err := c.Promoted(p, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.TextBody, "nothing else you need to do") {
		t.Error("the promotion email should make clear there is nothing to confirm")
	}
	if strings.Contains(strings.ToLower(msg.TextBody), "claim your spot") {
		t.Error("the promotion email implies a claim step that does not exist")
	}
}

func TestEveryEmailRendersBothParts(t *testing.T) {
	c := testContext()
	p := model.Player{Name: "David", Email: "david@example.com", Token: "u"}
	entry := model.Entry{RSVP: model.RSVP{Status: model.StatusConfirmed, Token: "tok"}, PlayerName: "David"}

	builders := map[string]func() (model.OutboxMessage, error){
		"invitation":   func() (model.OutboxMessage, error) { return c.Invitation(p, "tok") },
		"reminder":     func() (model.OutboxMessage, error) { return c.Reminder(p, "tok") },
		"confirmation": func() (model.OutboxMessage, error) { return c.Confirmation(p, entry) },
		"promoted":     func() (model.OutboxMessage, error) { return c.Promoted(p, entry) },
		"cancelled":    func() (model.OutboxMessage, error) { return c.Cancelled(p) },
		"organizer":    func() (model.OutboxMessage, error) { return c.OrganizerNotice(p, "David", 1) },
	}
	for name, build := range builders {
		msg, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.TrimSpace(msg.TextBody) == "" || strings.TrimSpace(msg.HTMLBody) == "" {
			t.Errorf("%s: missing a body part", name)
		}
		if msg.Subject == "" {
			t.Errorf("%s: missing a subject", name)
		}
		if !strings.Contains(msg.HTMLBody, "<table") {
			t.Errorf("%s: HTML part is not table-based, which breaks in Outlook", name)
		}
	}
}

func TestVenueAppearsInEveryRelevantEmail(t *testing.T) {
	c := testContext()
	p := model.Player{Name: "Diana", Email: "diana@example.com", Token: "u"}
	entry := model.Entry{RSVP: model.RSVP{Status: model.StatusConfirmed, Token: "tok"}, PlayerName: "Diana"}
	venue := "Bonsor Recreation Complex"

	builders := map[string]func() (model.OutboxMessage, error){
		"invitation":   func() (model.OutboxMessage, error) { return c.Invitation(p, "tok") },
		"reminder":     func() (model.OutboxMessage, error) { return c.Reminder(p, "tok") },
		"confirmation": func() (model.OutboxMessage, error) { return c.Confirmation(p, entry) },
		"promoted":     func() (model.OutboxMessage, error) { return c.Promoted(p, entry) },
	}
	for name, build := range builders {
		msg, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(msg.TextBody, venue) {
			t.Errorf("%s: plain-text body does not give the venue", name)
		}
		if !strings.Contains(msg.HTMLBody, venue) {
			t.Errorf("%s: HTML body does not give the venue", name)
		}
	}
}

func TestVenueIsOmittedWhenNotSet(t *testing.T) {
	c := testContext()
	c.Session.Venue = ""
	msg, err := c.Invitation(model.Player{Name: "D", Email: "d@example.com", Token: "u"}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.TextBody, "Where:") {
		t.Error("an empty venue should leave the row out entirely, not print a blank one")
	}
}

func TestRepliesReachTheOrganizer(t *testing.T) {
	// People answer these emails ("can't make it this week"), so a reply has to
	// land with the organizer rather than the SMTP account the app logs in as.
	c := testContext()
	p := model.Player{Name: "Diana", Email: "diana@example.com", Token: "u"}
	msg, err := c.Invitation(p, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if msg.ReplyTo != "alice@example.com" {
		t.Errorf("ReplyTo = %q, want the organizer address", msg.ReplyTo)
	}
}

func TestInvitationReadsAsCorrespondenceNotAMailshot(t *testing.T) {
	// The weekly invitation was being filed into Gmail's Promotions tab, where
	// the group does not see it. These are the structural signals that pushed
	// it there; the plain-text alternative and a single link are what keep it
	// looking like a note from a person.
	c := testContext()
	msg, err := c.Invitation(model.Player{Name: "Diana", Email: "diana@example.com", Token: "u"}, "tok")
	if err != nil {
		t.Fatal(err)
	}
	html := msg.HTMLBody

	if n := strings.Count(html, "<a "); n > 3 {
		t.Errorf("%d links in the invitation; a pile of links reads as marketing", n)
	}
	for _, marketing := range []string{"border-radius:8px;", "background-color:#0f5132;padding"} {
		if strings.Contains(html, marketing) {
			t.Errorf("invitation still contains a filled call-to-action button (%q)", marketing)
		}
	}
	if strings.Contains(html, "<img") {
		t.Error("invitation contains an image; image-heavy mail is classified as promotional")
	}
	if strings.TrimSpace(msg.TextBody) == "" {
		t.Error("invitation has no plain-text alternative")
	}
}
