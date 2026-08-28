// Package mail composes, queues and delivers the app's email. Nothing is sent
// inline from a request: messages are rendered here, queued in the outbox, and
// delivered by the worker.
package mail

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"strings"
	texttemplate "text/template"
	"time"

	"badminton/internal/model"
)

//go:embed templates/*
var templateFS embed.FS

var (
	htmlTmpl = template.Must(template.ParseFS(templateFS, "templates/email.html"))
	textTmpl = texttemplate.Must(texttemplate.ParseFS(templateFS, "templates/email.txt"))
)

// Row is one label/value line in the details table.
type Row struct{ Label, Value string }

// Data drives both the HTML and plain-text renderings. One template pair serves
// every message kind, so all six emails stay visually consistent for free.
type Data struct {
	Title          string
	Preheader      string
	RecipientName  string
	Intro          []string
	Badge          string
	BadgeColor     string
	Rows           []Row
	ActionURL      string
	ActionLabel    string
	ThingsToBring  string
	Notes          string
	SecondaryText  string
	OrganizerName  string
	ManageURL      string
	UnsubscribeURL string
}

// Render produces the HTML and plain-text bodies for a message.
func Render(d Data) (html, text string, err error) {
	var hb, tb bytes.Buffer
	if err := htmlTmpl.ExecuteTemplate(&hb, "email.html", d); err != nil {
		return "", "", fmt.Errorf("render html: %w", err)
	}
	if err := textTmpl.ExecuteTemplate(&tb, "email.txt", d); err != nil {
		return "", "", fmt.Errorf("render text: %w", err)
	}
	return hb.String(), tb.String(), nil
}

// Context carries everything the composers need about a session, so callers
// assemble it once per send rather than threading a dozen arguments around.
type Context struct {
	Session        model.Session
	Roster         model.Roster
	Loc            *time.Location
	BaseURL        string
	ThingsToBring  string
	OrganizerName  string
	OrganizerEmail string
}

// ManageURL is the participant's personal link: the signup page before they
// answer, the manage page afterwards.
func (c Context) ManageURL(token string) string { return c.BaseURL + "/r/" + token }

// PublicURL is the fallback signup link for forwarded invitations.
func (c Context) PublicURL() string { return c.BaseURL + "/s/" + c.Session.PublicID }

// UnsubscribeURL is a player's opt-out link.
func (c Context) UnsubscribeURL(playerToken string) string { return c.BaseURL + "/u/" + playerToken }

// DateLine renders the session date, e.g. "Tuesday, September 1".
func (c Context) DateLine() string {
	return c.Session.StartAt(c.Loc).Format("Monday, January 2")
}

// TimeLine renders the session hours, e.g. "7:00 PM - 9:00 PM".
func (c Context) TimeLine() string {
	s, e := c.Session.StartAt(c.Loc), c.Session.EndAt(c.Loc)
	return s.Format("3:04 PM") + " - " + e.Format("3:04 PM")
}

// DeadlineLine renders the signup deadline in the configured timezone.
func (c Context) DeadlineLine() string {
	return c.Session.SignupDeadline().In(c.Loc).Format("Monday, January 2 at 3:04 PM")
}

// SpotsLine describes how full the session is.
func (c Context) SpotsLine() string {
	r := c.Roster
	if r.IsFull() {
		if n := len(r.Waitlist); n > 0 {
			return fmt.Sprintf("%d of %d filled - full, %d on the waitlist",
				r.Headcount, c.Session.MaxPlayers, n)
		}
		return fmt.Sprintf("%d of %d filled - full", r.Headcount, c.Session.MaxPlayers)
	}
	return fmt.Sprintf("%d of %d filled - %d %s left",
		r.Headcount, c.Session.MaxPlayers, r.SpotsLeft, plural(r.SpotsLeft, "spot", "spots"))
}

// CostLine states the court rate and how it is shared. The wording is shared
// with the RSVP page so the two cannot drift.
func (c Context) CostLine() string { return c.Session.RateLine() }

func (c Context) sessionRows(includeSpots bool) []Row {
	rows := []Row{
		{Label: "Date", Value: c.DateLine()},
		{Label: "Time", Value: c.TimeLine()},
	}
	// Plain text, not a map link: mail clients linkify an address on their own,
	// and an extra tracking-shaped URL is one more reason to be filed as bulk.
	if v := strings.TrimSpace(c.Session.Venue); v != "" {
		rows = append(rows, Row{Label: "Where", Value: v})
	}
	rows = append(rows, Row{Label: "Courts", Value: fmt.Sprintf("%d", c.Session.Courts)})
	if includeSpots {
		rows = append(rows, Row{Label: "Spots", Value: c.SpotsLine()})
	}
	rows = append(rows,
		Row{Label: "Sign up by", Value: c.DeadlineLine()},
		Row{Label: "Cost", Value: c.CostLine()},
	)
	return rows
}

func (c Context) message(kind string, p model.Player, subject string, d Data) (model.OutboxMessage, error) {
	d.OrganizerName = c.OrganizerName
	d.RecipientName = firstName(p.Name)
	if d.UnsubscribeURL == "" {
		d.UnsubscribeURL = c.UnsubscribeURL(p.Token)
	}
	html, text, err := Render(d)
	if err != nil {
		return model.OutboxMessage{}, err
	}
	sessID := c.Session.ID
	playerID := p.ID
	return model.OutboxMessage{
		Kind:           kind,
		SessionID:      &sessID,
		PlayerID:       &playerID,
		ToEmail:        p.Email,
		ToName:         p.Name,
		Subject:        subject,
		ReplyTo:        c.OrganizerEmail,
		HTMLBody:       html,
		TextBody:       text,
		UnsubscribeURL: d.UnsubscribeURL,
	}, nil
}

// Invitation is the weekly email that starts the whole flow.
func (c Context) Invitation(p model.Player, token string) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	return c.message(model.KindInvitation, p, title, Data{
		Title:     title,
		Preheader: fmt.Sprintf("%s. %s.", c.TimeLine(), c.SpotsLine()),
		Intro: []string{
			"Badminton is on. Here are the details:",
		},
		Rows:          c.sessionRows(true),
		ActionURL:     c.ManageURL(token),
		ActionLabel:   "Sign up for this session",
		ThingsToBring: c.ThingsToBring,
		Notes:         c.Session.Notes,
		SecondaryText: "That link is yours alone, and it is also how you change or cancel your RSVP later.",
	})
}

// Reminder nudges people who have not answered yet.
func (c Context) Reminder(p model.Player, token string) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	action := "Sign Up"
	if c.Roster.IsFull() {
		action = "Join Waitlist"
	}
	return c.message(model.KindReminder, p, "Reminder: "+title, Data{
		Title:     title,
		Preheader: c.SpotsLine(),
		Intro: []string{
			fmt.Sprintf("We currently have %s.", c.SpotsLine()),
			fmt.Sprintf("If you would like to play, please sign up before %s.", c.DeadlineLine()),
		},
		Rows:          c.sessionRows(false),
		ActionURL:     c.ManageURL(token),
		ActionLabel:   action,
		ThingsToBring: c.ThingsToBring,
		Notes:         c.Session.Notes,
	})
}

// Confirmation goes out immediately after someone signs up and carries the
// permanent link they will use to change or cancel.
func (c Context) Confirmation(p model.Player, e model.Entry) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	badge, color, subject := "You're confirmed", "#0f5132", "You're in: "+title
	intro := []string{"Thanks - you are on the list."}
	if e.Status == model.StatusWaitlist {
		badge, color, subject = "You're on the waitlist", "#9a6700", "Waitlisted: "+title
		intro = []string{
			"The session is full at the moment, so you are on the waitlist.",
			"If someone drops out you will get an email letting you know you are in.",
		}
	}
	if e.GuestCount > 0 {
		intro = append(intro, fmt.Sprintf("You are booked for %d %s including your %s.",
			e.PartySize(), plural(e.PartySize(), "spot", "spots"),
			plural(e.GuestCount, "guest", "guests")))
	}
	return c.message(model.KindConfirmation, p, subject, Data{
		Title:         title,
		Preheader:     badge,
		Badge:         badge,
		BadgeColor:    color,
		Intro:         intro,
		Rows:          c.sessionRows(false),
		ActionURL:     c.ManageURL(e.Token),
		ActionLabel:   "Manage my RSVP",
		ThingsToBring: c.ThingsToBring,
		Notes:         c.Session.Notes,
		SecondaryText: "Plans changed? Use the link above to add a guest or cancel.",
	})
}

// Promoted tells someone the organizer moved them off the waitlist. They are
// already confirmed - there is no spot to claim and nothing to time out.
func (c Context) Promoted(p model.Player, e model.Entry) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	return c.message(model.KindPromoted, p, "A spot has opened up: "+title, Data{
		Title:      title,
		Preheader:  "A spot has opened up and you are in.",
		Badge:      "You're confirmed",
		BadgeColor: "#0f5132",
		Intro: []string{
			"A spot has opened up for badminton and you are off the waitlist.",
			"You are confirmed - there is nothing else you need to do.",
		},
		Rows:          c.sessionRows(false),
		ActionURL:     c.ManageURL(e.Token),
		ActionLabel:   "Manage my RSVP",
		ThingsToBring: c.ThingsToBring,
		Notes:         c.Session.Notes,
		SecondaryText: "If you can no longer make it, please cancel with the link above so someone else can play.",
	})
}

// Cancelled tells participants the session is off.
func (c Context) Cancelled(p model.Player) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	return c.message(model.KindCancelled, p, "Cancelled: "+title, Data{
		Title:      title,
		Preheader:  "This session has been cancelled.",
		Badge:      "Cancelled",
		BadgeColor: "#b42318",
		Intro: []string{
			"This badminton session has been cancelled.",
			"Sorry for the short notice - see you next time.",
		},
		Rows:  c.sessionRows(false),
		Notes: c.Session.Notes,
	})
}

// OrganizerNotice tells the organizer what just changed on the roster, in the
// terms they act on: who, and how many spots moved. Cancellations and dropped
// guests are the ones that may need a promotion from the waitlist, so the
// subject says so plainly rather than burying it.
func (c Context) OrganizerNotice(to model.Player, ch model.RosterChange) (model.OutboxMessage, error) {
	title := "Badminton - " + c.DateLine()
	headline, subject := describeChange(ch)

	intro := []string{headline}
	if ch.FreesSpots() {
		if n := len(c.Roster.Waitlist); n > 0 {
			intro = append(intro, fmt.Sprintf("There %s %d %s on the waitlist.",
				plural(n, "is", "are"), n, plural(n, "person", "people")))
		} else {
			intro = append(intro, "Nobody is on the waitlist.")
		}
	}

	return c.message(model.KindOrganizer, to, subject+" - "+title, Data{
		Title:       title,
		Preheader:   headline,
		Intro:       intro,
		Rows:        c.sessionRows(true),
		ActionURL:   c.BaseURL + fmt.Sprintf("/sessions/%d", c.Session.ID),
		ActionLabel: "Open session dashboard",
	})
}

// describeChange turns a roster change into a sentence and a subject prefix.
func describeChange(ch model.RosterChange) (headline, subject string) {
	switch ch.Action {
	case model.ChangeSignedUp:
		if ch.GuestsAfter > 0 {
			return fmt.Sprintf("%s signed up with %d %s, taking %d %s.",
					ch.Who, ch.GuestsAfter, plural(ch.GuestsAfter, "guest", "guests"),
					ch.SpotsAfter, plural(ch.SpotsAfter, "spot", "spots")),
				ch.Who + " signed up"
		}
		return fmt.Sprintf("%s signed up.", ch.Who), ch.Who + " signed up"

	case model.ChangeWaitlisted:
		return fmt.Sprintf("%s joined the waitlist.", ch.Who), ch.Who + " joined the waitlist"

	case model.ChangeCancelled:
		if ch.SpotsBefore > 0 {
			return fmt.Sprintf("%s cancelled, freeing %d %s.",
					ch.Who, ch.SpotsBefore, plural(ch.SpotsBefore, "spot", "spots")),
				ch.Who + " cancelled"
		}
		return fmt.Sprintf("%s cancelled from the waitlist.", ch.Who), ch.Who + " cancelled"

	case model.ChangeGuests:
		// The guest count is what changed, so say it the way the organizer
		// would: "+1 to +0", not "party size 2 to 1".
		before, after := guestLabel(ch.GuestsBefore), guestLabel(ch.GuestsAfter)
		if ch.FreesSpots() {
			n := ch.SpotsFreed()
			return fmt.Sprintf("%s changed from %s to %s, freeing %d %s.",
					ch.Who, before, after, n, plural(n, "spot", "spots")),
				fmt.Sprintf("%s dropped to %s", ch.Who, after)
		}
		n := ch.SpotsAfter - ch.SpotsBefore
		return fmt.Sprintf("%s changed from %s to %s, taking %d more %s.",
				ch.Who, before, after, n, plural(n, "spot", "spots")),
			fmt.Sprintf("%s went to %s", ch.Who, after)
	}
	return ch.Who + " changed their RSVP.", ch.Who + " changed their RSVP"
}

func guestLabel(guests int) string { return fmt.Sprintf("+%d", guests) }

// TestMessage verifies SMTP settings without involving a session.
func TestMessage(baseURL, organizerName, to string) (model.OutboxMessage, error) {
	d := Data{
		Title:         "SMTP test",
		Preheader:     "Your badminton app can send email.",
		RecipientName: "there",
		Intro: []string{
			"This is a test message from your badminton app.",
			"If you are reading it, SMTP is configured correctly.",
		},
		Rows:          []Row{{Label: "App address", Value: baseURL}},
		OrganizerName: organizerName,
	}
	html, text, err := Render(d)
	if err != nil {
		return model.OutboxMessage{}, err
	}
	return model.OutboxMessage{
		Kind: model.KindTest, ToEmail: to, Subject: "Badminton app - SMTP test",
		HTMLBody: html, TextBody: text,
	}, nil
}

func firstName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "there"
	}
	if i := strings.IndexAny(name, " \t"); i > 0 {
		return name[:i]
	}
	return name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
