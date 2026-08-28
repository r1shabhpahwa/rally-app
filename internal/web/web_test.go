package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"badminton/internal/config"
	"badminton/internal/mail"
	"badminton/internal/model"
	"badminton/internal/store"
)

// The whole participant flow runs against a real HTTP server, real templates
// and a real database, with only SMTP replaced.

type harness struct {
	t      *testing.T
	srv    *Server
	store  *store.Store
	http   *httptest.Server
	client *http.Client
	csrf   string
}

var fixedNow = mustParse("2026-08-28 10:00")

func mustParse(s string) time.Time {
	loc, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		panic(err)
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	loc, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.EnsureOrganizer(context.Background(), "Alice Smith", "alice@example.com", "badminton123"); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		BaseURL: "http://badminton.test", Timezone: loc,
		OrganizerName: "Alice Smith", OrganizerEmail: "alice@example.com",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := mail.NewWorker(st, mail.LogSender{Log: log}, log, 100)

	srv, err := New(cfg, st, worker, log)
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return fixedNow }

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	h := &harness{t: t, srv: srv, store: st, http: ts, client: &http.Client{Jar: jar}}
	return h
}

func (h *harness) get(path string) (int, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.http.URL + path)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (h *harness) post(path string, form url.Values) (int, string) {
	h.t.Helper()
	if h.csrf != "" && form.Get("csrf") == "" {
		form.Set("csrf", h.csrf)
	}
	resp, err := h.client.PostForm(h.http.URL+path, form)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (h *harness) login() {
	h.t.Helper()
	if code, _ := h.post("/login", url.Values{"password": {"badminton123"}}); code != http.StatusOK {
		h.t.Fatalf("login returned %d", code)
	}
	_, body := h.get("/players")
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatal("no CSRF token found on an authenticated page")
	}
	h.csrf = m[1]
}

// tokens maps each recipient's email to the personal RSVP link found in the
// message that was queued for them.
var tokenRe = regexp.MustCompile(`/r/([A-Za-z0-9_-]{20,})`)

func (h *harness) invitationTokens() map[string]string {
	h.t.Helper()
	msgs, err := h.store.RecentMessages(context.Background(), 500)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range msgs {
		if m.Kind != model.KindInvitation {
			continue
		}
		if match := tokenRe.FindStringSubmatch(m.TextBody); match != nil {
			out[m.ToEmail] = match[1]
		}
	}
	return out
}

func (h *harness) messagesOfKind(kind string) []model.OutboxMessage {
	h.t.Helper()
	msgs, err := h.store.RecentMessages(context.Background(), 500)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []model.OutboxMessage
	for _, m := range msgs {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) roster(sessionID int64) model.Roster {
	h.t.Helper()
	sess, err := h.store.Session(context.Background(), sessionID)
	if err != nil || sess == nil {
		h.t.Fatalf("load session: %v", err)
	}
	r, err := h.store.Roster(context.Background(), *sess)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// setupSession imports five players and creates a three-person session.
func (h *harness) setupSession(maxPlayers string) int64 {
	h.t.Helper()
	h.post("/players/import", url.Values{"pasted": {
		"name,email\nAlice Smith,alice@example.com\nDavid,david@example.com\n" +
			"Diana,diana@example.com\nEd,ed@example.com\nSarah,sarah@example.com",
	}})

	code, _ := h.post("/sessions/new", url.Values{
		"date": {"2026-09-01"}, "start_time": {"19:00"}, "end_time": {"21:00"},
		"courts": {"3"}, "cost": {"35.00"}, "max_players": {maxPlayers},
		"deadline_date": {"2026-08-29"}, "deadline_time": {"15:00"},
	})
	if code != http.StatusOK {
		h.t.Fatalf("create session returned %d", code)
	}
	sessions, err := h.store.Sessions(context.Background(), "2026-01-01", false)
	if err != nil || len(sessions) != 1 {
		h.t.Fatalf("expected exactly one session, got %d (%v)", len(sessions), err)
	}
	return sessions[0].ID
}

func TestFullWeeklyWorkflow(t *testing.T) {
	h := newHarness(t)
	h.login()
	sessionID := h.setupSession("3")

	// --- Invitation: one personalised message per mailing-list member. ---
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()
	if len(tokens) != 5 {
		t.Fatalf("queued %d invitations, want 5", len(tokens))
	}
	seen := map[string]bool{}
	for email, token := range tokens {
		if seen[token] {
			t.Fatalf("%s received a token that was reused for someone else", email)
		}
		seen[token] = true
	}

	// --- Signup with a guest takes two spots. ---
	code, body := h.get("/r/" + tokens["alice@example.com"])
	if code != http.StatusOK || !strings.Contains(body, "Alice Smith") {
		t.Fatalf("signup page: code %d, name prefilled: %v", code, strings.Contains(body, "Alice Smith"))
	}
	h.post("/r/"+tokens["alice@example.com"]+"/signup", url.Values{
		"name": {"Alice Smith"}, "guest_count": {"1"},
	})
	if r := h.roster(sessionID); r.Headcount != 2 {
		t.Fatalf("headcount = %d after a player+guest signed up, want 2", r.Headcount)
	}

	// --- Filling the last spot makes the session full. ---
	h.post("/r/"+tokens["david@example.com"]+"/signup", url.Values{"name": {"David"}})
	r := h.roster(sessionID)
	if r.Headcount != 3 || !r.IsFull() {
		t.Fatalf("headcount = %d, full = %v; want 3 and full", r.Headcount, r.IsFull())
	}

	// --- A full session offers the waitlist rather than refusing. ---
	_, body = h.get("/r/" + tokens["diana@example.com"])
	if !strings.Contains(body, "Join waitlist") {
		t.Fatal("a full session should offer to join the waitlist")
	}
	h.post("/r/"+tokens["diana@example.com"]+"/signup", url.Values{"name": {"Diana"}})
	if r := h.roster(sessionID); len(r.Waitlist) != 1 || r.Headcount != 3 {
		t.Fatalf("waitlist = %d, headcount = %d; want 1 and 3", len(r.Waitlist), r.Headcount)
	}

	// Everyone who signed up got their permanent link.
	if got := len(h.messagesOfKind(model.KindConfirmation)); got != 3 {
		t.Fatalf("confirmation emails = %d, want 3", got)
	}

	// --- Cancelling frees the spots and tells the organizer. ---
	h.post("/r/"+tokens["alice@example.com"]+"/cancel", url.Values{})
	r = h.roster(sessionID)
	if r.Headcount != 1 || r.SpotsLeft != 2 {
		t.Fatalf("after cancel: headcount %d, spots left %d; want 1 and 2", r.Headcount, r.SpotsLeft)
	}
	if len(r.Waitlist) != 1 {
		t.Fatal("cancelling must not auto-promote: the organizer decides")
	}
	notices := h.messagesOfKind(model.KindOrganizer)
	var cancelNotice *model.OutboxMessage
	for i, n := range notices {
		if n.ToEmail != "alice@example.com" {
			t.Fatalf("organizer notice addressed to %q, want the organizer", n.ToEmail)
		}
		if strings.Contains(n.Subject, "cancelled") {
			cancelNotice = &notices[i]
		}
	}
	if cancelNotice == nil {
		t.Fatalf("no cancellation notice reached the organizer; got %d notices", len(notices))
	}
	if !strings.Contains(cancelNotice.TextBody, "freeing 2 spots") {
		t.Error("the cancellation notice does not say how much capacity came back")
	}

	// --- Promotion confirms outright and emails the person. ---
	h.post("/sessions/1/promote", url.Values{})
	r = h.roster(sessionID)
	if r.Headcount != 2 || len(r.Waitlist) != 0 {
		t.Fatalf("after promotion: headcount %d, waitlist %d; want 2 and 0", r.Headcount, len(r.Waitlist))
	}
	promotions := h.messagesOfKind(model.KindPromoted)
	if len(promotions) != 1 || promotions[0].ToEmail != "diana@example.com" {
		t.Fatalf("promotion emails = %+v, want one to Diana", promotions)
	}
	for _, e := range r.Confirmed {
		if e.PlayerName == "Diana" && e.Status != model.StatusConfirmed {
			t.Fatal("a promoted player must be confirmed outright, with no spot to claim")
		}
	}

	// --- Cost: 3 courts x 2h x $35 = $210, split across bodies. ---
	_, body = h.get("/sessions/1")
	if !strings.Contains(body, "$210.00") {
		t.Fatal("session dashboard does not show the $210.00 court total")
	}
	if !strings.Contains(body, "$105.00") {
		t.Fatal("session dashboard does not show $105.00 per player for 2 players")
	}
}

func TestGuestsCountTowardsCostSplit(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	// Two people, one of them with a guest: three bodies paying $210.
	h.post("/r/"+tokens["alice@example.com"]+"/signup", url.Values{"name": {"Alice Smith"}, "guest_count": {"1"}})
	h.post("/r/"+tokens["david@example.com"]+"/signup", url.Values{"name": {"David"}})

	_, body := h.get("/sessions/1")
	if !strings.Contains(body, "$70.00") {
		t.Fatal("the guest is not being counted in the per-player split ($210 / 3 = $70.00)")
	}
}

func TestParticipantCannotSeeOrChangeAnotherRSVP(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	h.post("/r/"+tokens["diana@example.com"]+"/signup", url.Values{"name": {"Diana"}})

	// David's page must be about David, with no way to touch Diana's RSVP.
	_, body := h.get("/r/" + tokens["david@example.com"])
	if strings.Contains(body, "Diana") {
		t.Fatal("one participant's page exposes another participant's RSVP")
	}
	if strings.Contains(body, tokens["diana@example.com"]) {
		t.Fatal("one participant's page leaks another participant's token")
	}

	// An unknown token is a dead end, not an error page with detail.
	code, _ := h.get("/r/" + strings.Repeat("a", 43))
	if code != http.StatusNotFound {
		t.Fatalf("unknown token returned %d, want 404", code)
	}
}

func TestSignupRefusedAfterDeadline(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	// Move past the Saturday 3pm cut-off.
	h.srv.now = func() time.Time { return mustParse("2026-08-30 10:00") }

	_, body := h.get("/r/" + tokens["david@example.com"])
	if !strings.Contains(body, "Signups are closed") {
		t.Fatal("the RSVP page should say signups are closed once the deadline passes")
	}
	h.post("/r/"+tokens["david@example.com"]+"/signup", url.Values{"name": {"David"}})
	if r := h.roster(1); r.Headcount != 0 {
		t.Fatalf("headcount = %d, want 0: a late signup must not be accepted", r.Headcount)
	}
}

func TestReminderTargetsNonRespondersByDefault(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()
	h.post("/r/"+tokens["alice@example.com"]+"/signup", url.Values{"name": {"Alice Smith"}})

	h.post("/sessions/1/remind", url.Values{})
	reminders := h.messagesOfKind(model.KindReminder)
	if len(reminders) != 4 {
		t.Fatalf("reminders = %d, want 4 (everyone except the one who replied)", len(reminders))
	}
	for _, m := range reminders {
		if m.ToEmail == "alice@example.com" {
			t.Fatal("someone who already replied was reminded to sign up")
		}
	}

	// The opt-in toggle reaches everyone.
	h.post("/sessions/1/remind", url.Values{"include_all": {"1"}})
	if got := len(h.messagesOfKind(model.KindReminder)); got != 4+5 {
		t.Fatalf("reminders after 'send to everyone' = %d, want 9", got)
	}
}

func TestResendInvitationRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")

	h.post("/sessions/1/invite", url.Values{})
	if got := len(h.messagesOfKind(model.KindInvitation)); got != 5 {
		t.Fatalf("invitations = %d, want 5", got)
	}
	// A second click must not double every message.
	h.post("/sessions/1/invite", url.Values{})
	if got := len(h.messagesOfKind(model.KindInvitation)); got != 5 {
		t.Fatalf("invitations after a second click = %d, want 5 (double-send guard)", got)
	}
	// Explicit confirmation does resend.
	h.post("/sessions/1/invite", url.Values{"confirm_resend": {"1"}})
	if got := len(h.messagesOfKind(model.KindInvitation)); got != 10 {
		t.Fatalf("invitations after a confirmed resend = %d, want 10", got)
	}
}

func TestPublicFallbackSignupCreatesInactivePlayer(t *testing.T) {
	h := newHarness(t)
	h.login()
	sessionID := h.setupSession("12")
	sess, _ := h.store.Session(context.Background(), sessionID)

	// Someone who was forwarded the email and is not on the mailing list.
	code, _ := h.post("/s/"+sess.PublicID, url.Values{
		"name": {"Forwarded Friend"}, "email": {"friend@example.com"}, "guest_count": {"0"},
	})
	if code != http.StatusOK {
		t.Fatalf("public signup returned %d", code)
	}
	if r := h.roster(sessionID); r.Headcount != 1 {
		t.Fatalf("headcount = %d, want 1", r.Headcount)
	}
	player, err := h.store.PlayerByEmail(context.Background(), "friend@example.com")
	if err != nil || player == nil {
		t.Fatalf("player not created: %v", err)
	}
	if player.Active {
		t.Fatal("a public signup must not silently add someone to the weekly mailing list")
	}
	mailable, _ := h.store.MailablePlayers(context.Background())
	if len(mailable) != 5 {
		t.Fatalf("mailing list = %d, want the original 5", len(mailable))
	}
}

func TestOrganizerRoutesRequireLogin(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/players", "/settings", "/outbox", "/sessions/new"} {
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}).Get(h.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s returned %d without a session, want a redirect to /login", path, resp.StatusCode)
		}
	}
}

func TestPostWithoutCSRFIsRejected(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")

	// Same session cookie, no CSRF field: the request must not take effect.
	h.post("/players/new", url.Values{"csrf": {"wrong"}, "name": {"Mallory"}, "email": {"m@example.com"}})
	players, _ := h.store.Players(context.Background(), "")
	for _, p := range players {
		if p.Email == "m@example.com" {
			t.Fatal("a POST with a bad CSRF token was accepted")
		}
	}
}

func TestUnsubscribeStopsFutureEmail(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")

	player, err := h.store.PlayerByEmail(context.Background(), "ed@example.com")
	if err != nil || player == nil {
		t.Fatal(err)
	}
	if code, _ := h.post("/u/"+player.Token, url.Values{}); code != http.StatusOK {
		t.Fatal("unsubscribe page did not accept the request")
	}

	h.post("/sessions/1/invite", url.Values{})
	for _, m := range h.messagesOfKind(model.KindInvitation) {
		if m.ToEmail == "ed@example.com" {
			t.Fatal("an unsubscribed player still received the invitation")
		}
	}
	if got := len(h.messagesOfKind(model.KindInvitation)); got != 4 {
		t.Fatalf("invitations = %d, want 4", got)
	}
}

func TestManualAddIsTheEscapeHatch(t *testing.T) {
	h := newHarness(t)
	h.login()
	sessionID := h.setupSession("12")

	// Past the deadline, someone texts the organizer instead of clicking.
	h.srv.now = func() time.Time { return mustParse("2026-08-30 10:00") }
	h.post("/sessions/1/participants", url.Values{
		"player_id": {"new"}, "name": {"Texted Me"}, "email": {"texted@example.com"}, "guest_count": {"0"},
	})
	if r := h.roster(sessionID); r.Headcount != 1 {
		t.Fatalf("headcount = %d, want 1: the organizer must be able to add people after the deadline", r.Headcount)
	}
	if got := len(h.messagesOfKind(model.KindConfirmation)); got != 1 {
		t.Fatalf("confirmations = %d, want the manually added person to get their link", got)
	}
}

func TestCancellingSessionNotifiesEveryone(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("2")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	h.post("/r/"+tokens["alice@example.com"]+"/signup", url.Values{"name": {"Alice Smith"}})
	h.post("/r/"+tokens["david@example.com"]+"/signup", url.Values{"name": {"David"}})
	h.post("/r/"+tokens["diana@example.com"]+"/signup", url.Values{"name": {"Diana"}}) // waitlisted

	h.post("/sessions/1/status", url.Values{"status": {"cancelled"}})
	notices := h.messagesOfKind(model.KindCancelled)
	if len(notices) != 3 {
		t.Fatalf("cancellation notices = %d, want 3 (confirmed and waitlisted alike)", len(notices))
	}
}

func TestTokensAreNotWrittenToLogs(t *testing.T) {
	// Tokens in a log file are working credentials for someone else's RSVP.
	if got := redactPath("/r/abc123/cancel"); got != "/r/[token]/cancel" {
		t.Fatalf("redactPath = %q, want the token replaced", got)
	}
	if got := redactPath("/u/secrettoken"); got != "/u/[token]" {
		t.Fatalf("redactPath = %q, want the token replaced", got)
	}
	if got := redactPath("/sessions/1"); got != "/sessions/1" {
		t.Fatalf("redactPath rewrote an ordinary path: %q", got)
	}
}

// A participant's token lives in the URL, so the pages that hang off it must
// tell the browser not to put that URL in a Referer header, and must stay out
// of search indexes. This is asserted on a real response rather than assumed,
// because the headers are set on the way out and are easy to lose: writing the
// status first silently discards everything set after it.
func TestRSVPPagesDoNotLeakTheTokenViaReferer(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	check := func(label, path string, wantStatus int) {
		t.Helper()
		resp, err := h.client.Get(h.http.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)

		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status %d, want %d", label, resp.StatusCode, wantStatus)
		}
		if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q, want no-referrer - the token would leak to other sites", label, got)
		}
		if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s: X-Robots-Tag = %q, want noindex", label, got)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s: Content-Type = %q, want text/html", label, ct)
		}
	}

	check("valid RSVP page", "/r/"+tokens["david@example.com"], http.StatusOK)
	// The 404 matters too: a stale or mistyped link still carries a token-shaped
	// path, and its Referer would carry it onward.
	check("unknown token 404", "/r/"+strings.Repeat("a", 43), http.StatusNotFound)
	check("public signup page", "/s/"+func() string {
		sess, _ := h.store.Session(context.Background(), 1)
		return sess.PublicID
	}(), http.StatusOK)
}

func TestVenueFlowsFromSettingsToSessionToParticipantAndEmail(t *testing.T) {
	h := newHarness(t)
	h.login()
	venue := "Bonsor Recreation Complex, 6550 Bonsor Ave, Burnaby"

	// Saving it as a default is what makes the weekly session a couple of clicks.
	h.post("/settings", url.Values{
		"venue": {venue}, "courts": {"3"}, "cost": {"35.00"}, "max_players": {"12"},
		"start_time": {"19:00"}, "end_time": {"21:00"},
		"deadline_days_before": {"3"}, "deadline_time": {"15:00"},
		"things_to_bring": {"Racquet and shoes"},
	})
	_, form := h.get("/sessions/new")
	if !strings.Contains(form, venue) {
		t.Fatal("the new-session form does not prefill the venue from settings")
	}

	h.post("/players/import", url.Values{"pasted": {"David,david@example.com"}})
	h.post("/sessions/new", url.Values{
		"date": {"2026-09-01"}, "start_time": {"19:00"}, "end_time": {"21:00"},
		"venue": {venue}, "courts": {"3"}, "cost": {"35.00"}, "max_players": {"12"},
		"deadline_date": {"2026-08-29"}, "deadline_time": {"15:00"},
	})

	sess, err := h.store.Session(context.Background(), 1)
	if err != nil || sess == nil {
		t.Fatalf("session not created: %v", err)
	}
	if sess.Venue != venue {
		t.Fatalf("stored venue = %q, want %q", sess.Venue, venue)
	}

	// It has to reach the two places people actually look.
	if _, body := h.get("/sessions/1"); !strings.Contains(body, "Bonsor") {
		t.Error("the organizer dashboard does not show the venue")
	}
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()
	if _, body := h.get("/r/" + tokens["david@example.com"]); !strings.Contains(body, "Bonsor") {
		t.Error("the participant's RSVP page does not show the venue")
	}
	for _, m := range h.messagesOfKind(model.KindInvitation) {
		if !strings.Contains(m.TextBody, "Bonsor") || !strings.Contains(m.HTMLBody, "Bonsor") {
			t.Error("the invitation email does not include the venue in both parts")
		}
		if m.ReplyTo == "" {
			t.Error("the invitation has no Reply-To, so replies would not reach the organizer")
		}
	}

	// Editing a session must be able to override the default for a one-off.
	other := "Kensington Community Centre"
	h.post("/sessions/1/edit", url.Values{
		"date": {"2026-09-01"}, "start_time": {"19:00"}, "end_time": {"21:00"},
		"venue": {other}, "courts": {"3"}, "cost": {"35.00"}, "max_players": {"12"},
		"deadline_date": {"2026-08-29"}, "deadline_time": {"15:00"},
	})
	sess, _ = h.store.Session(context.Background(), 1)
	if sess.Venue != other {
		t.Fatalf("venue after edit = %q, want the override %q", sess.Venue, other)
	}
}

func TestParticipantPagesQuoteTheRateNotAMovingTotal(t *testing.T) {
	// Same reasoning as the emails: an invitation reaches an empty roster, so a
	// per-player figure shown to someone deciding whether to play is several
	// times the real one. The wording is shared with the email so they cannot
	// drift apart.
	h := newHarness(t)
	h.login()
	sessionID := h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()

	sess, _ := h.store.Session(context.Background(), sessionID)
	pages := map[string]string{
		"RSVP page":          "/r/" + tokens["david@example.com"],
		"public signup page": "/s/" + sess.PublicID,
	}
	for label, path := range pages {
		_, body := h.get(path)
		if !strings.Contains(body, "$35 per court per hour, divided by the number of players") {
			t.Errorf("%s does not quote the court rate", label)
		}
		for _, moving := range []string{"$210.00 total", "about $105.00 each", "$105.00 each"} {
			if strings.Contains(body, moving) {
				t.Errorf("%s still shows a figure that moves as people sign up: %q", label, moving)
			}
		}
	}

	// The organizer still needs the real numbers, and they recalculate per view.
	_, dash := h.get("/sessions/1")
	if !strings.Contains(dash, "$210.00") {
		t.Error("the organizer dashboard should still show the court total")
	}
}

// organizerNotices returns the subjects of everything addressed to the organizer.
func (h *harness) organizerNotices() []string {
	h.t.Helper()
	var out []string
	for _, m := range h.messagesOfKind(model.KindOrganizer) {
		if m.ToEmail != "alice@example.com" {
			h.t.Fatalf("organizer notice addressed to %q", m.ToEmail)
		}
		out = append(out, m.Subject)
	}
	return out
}

func containsSub(subjects []string, want string) bool {
	for _, s := range subjects {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func TestOrganizerIsToldAboutEveryRosterChange(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/invite", url.Values{})
	tokens := h.invitationTokens()
	david := tokens["david@example.com"]

	// Signing up.
	h.post("/r/"+david+"/signup", url.Values{"name": {"David"}, "guest_count": {"1"}})
	if subs := h.organizerNotices(); !containsSub(subs, "David signed up") {
		t.Fatalf("no signup notice reached the organizer; got %v", subs)
	}

	// Dropping the guest: +1 becomes +0. This is the case that was missing.
	h.post("/r/"+david+"/guest", url.Values{"guest_count": {"0"}})
	if subs := h.organizerNotices(); !containsSub(subs, "David dropped to +0") {
		t.Fatalf("no guest-drop notice; got %v", subs)
	}
	for _, m := range h.messagesOfKind(model.KindOrganizer) {
		if strings.Contains(m.Subject, "dropped to +0") &&
			!strings.Contains(m.TextBody, "changed from +1 to +0, freeing 1 spot") {
			t.Errorf("the guest-drop notice does not say what changed:\n%s", m.TextBody)
		}
	}

	// Cancelling outright.
	h.post("/r/"+david+"/cancel", url.Values{})
	if subs := h.organizerNotices(); !containsSub(subs, "David cancelled") {
		t.Fatalf("no cancellation notice; got %v", subs)
	}
}

func TestOrganizerNotificationLevelIsRespected(t *testing.T) {
	// Every signup emailing the organizer is thirty emails in a busy week,
	// which is the problem this app exists to remove. The level has to work.
	settings := func(level string) url.Values {
		return url.Values{
			"organizer_notify": {level},
			"courts":           {"3"}, "cost": {"35.00"}, "max_players": {"12"},
			"start_time": {"19:00"}, "end_time": {"21:00"},
			"deadline_days_before": {"3"}, "deadline_time": {"15:00"},
		}
	}

	t.Run("freed only", func(t *testing.T) {
		h := newHarness(t)
		h.login()
		h.setupSession("12")
		h.post("/settings", settings("freed"))
		h.post("/sessions/1/invite", url.Values{})
		david := h.invitationTokens()["david@example.com"]

		h.post("/r/"+david+"/signup", url.Values{"name": {"David"}, "guest_count": {"1"}})
		if subs := h.organizerNotices(); len(subs) != 0 {
			t.Fatalf("a signup notified the organizer at the 'freed' level: %v", subs)
		}
		// Dropping a guest gives capacity back, so this one must arrive.
		h.post("/r/"+david+"/guest", url.Values{"guest_count": {"0"}})
		if subs := h.organizerNotices(); !containsSub(subs, "dropped to +0") {
			t.Fatalf("a freed spot did not notify at the 'freed' level: %v", subs)
		}
	})

	t.Run("none", func(t *testing.T) {
		h := newHarness(t)
		h.login()
		h.setupSession("12")
		h.post("/settings", settings("none"))
		h.post("/sessions/1/invite", url.Values{})
		david := h.invitationTokens()["david@example.com"]

		h.post("/r/"+david+"/signup", url.Values{"name": {"David"}})
		h.post("/r/"+david+"/cancel", url.Values{})
		if subs := h.organizerNotices(); len(subs) != 0 {
			t.Fatalf("notices were sent at the 'none' level: %v", subs)
		}
	})
}

func TestOrganizerAddingSomeoneDoesNotEmailThemselves(t *testing.T) {
	// They did it; telling them about it is noise.
	h := newHarness(t)
	h.login()
	h.setupSession("12")
	h.post("/sessions/1/participants", url.Values{
		"player_id": {"new"}, "name": {"Texted Me"}, "email": {"texted@example.com"}, "guest_count": {"0"},
	})
	if subs := h.organizerNotices(); len(subs) != 0 {
		t.Fatalf("the organizer was emailed about their own action: %v", subs)
	}
}
