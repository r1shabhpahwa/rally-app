package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"badminton/internal/model"
)

// Errors surfaced to the participant and organizer UIs.
var (
	ErrNotFound      = errors.New("not found")
	ErrSignupsClosed = errors.New("signups are closed for this session")
	ErrNoRoom        = errors.New("there is not enough room for that party")
)

const rsvpCols = `r.id, r.session_id, r.player_id, r.status, r.guest_count, r.token,
	r.waitlist_position, r.source, r.created_at, r.updated_at,
	p.name, p.email, p.active`

func scanEntry(sc interface{ Scan(...any) error }) (model.Entry, error) {
	var e model.Entry
	var pos sql.NullInt64
	err := sc.Scan(&e.ID, &e.SessionID, &e.PlayerID, &e.Status, &e.GuestCount, &e.Token,
		&pos, &e.Source, &e.CreatedAt, &e.UpdatedAt,
		&e.PlayerName, &e.PlayerEmail, &e.PlayerActive)
	if pos.Valid {
		v := int(pos.Int64)
		e.WaitlistPosition = &v
	}
	return e, err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func entriesFor(ctx context.Context, q queryer, sessionID int64) ([]model.Entry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+rsvpCols+` FROM rsvp r JOIN player p ON p.id = r.player_id
		 WHERE r.session_id = ? ORDER BY r.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Entries returns every RSVP row for a session, joined to its player.
func (s *Store) Entries(ctx context.Context, sessionID int64) ([]model.Entry, error) {
	return entriesFor(ctx, s.db, sessionID)
}

// Roster loads a session's entries and computes the capacity and cost view.
func (s *Store) Roster(ctx context.Context, sess model.Session) (model.Roster, error) {
	entries, err := s.Entries(ctx, sess.ID)
	if err != nil {
		return model.Roster{}, err
	}
	return model.BuildRoster(sess, entries), nil
}

// EntryByToken resolves a participant's personal link to their RSVP and its
// session. This one token serves as both the signup link and the permanent
// manage link.
func (s *Store) EntryByToken(ctx context.Context, token string) (*model.Entry, *model.Session, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx,
		`SELECT `+rsvpCols+` FROM rsvp r JOIN player p ON p.id = r.player_id WHERE r.token = ?`, token))
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sess, err := s.Session(ctx, e.SessionID)
	if err != nil {
		return nil, nil, err
	}
	return &e, sess, nil
}

// EnsureInvites creates an invited RSVP row, with its own token, for every
// given player who does not have one yet. It is safe to call repeatedly: that
// is what lets a reminder top up people added to the mailing list after the
// invitation went out. It returns the entries for all supplied players.
func (s *Store) EnsureInvites(ctx context.Context, sessionID int64, players []model.Player) ([]model.Entry, error) {
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, p := range players {
			var exists int
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM rsvp WHERE session_id = ? AND player_id = ?`, sessionID, p.ID).Scan(&exists)
			if err == nil {
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rsvp (session_id, player_id, status, guest_count, token, source, created_at, updated_at)
				 VALUES (?, ?, ?, 0, ?, ?, unixepoch(), unixepoch())`,
				sessionID, p.ID, model.StatusInvited, NewToken(), model.SourceInvite); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	all, err := s.Entries(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	wanted := make(map[int64]bool, len(players))
	for _, p := range players {
		wanted[p.ID] = true
	}
	out := make([]model.Entry, 0, len(players))
	for _, e := range all {
		if wanted[e.PlayerID] {
			out = append(out, e)
		}
	}
	return out, nil
}

// Outcome describes what happened to a signup or promotion.
type Outcome struct {
	Entry      model.Entry
	Waitlisted bool
	Position   int
}

// Signup confirms or waitlists an RSVP. The capacity check and the write happen
// in one transaction, so two people cannot both take the last slot. Parties are
// all-or-nothing: a person plus a guest is confirmed together or waitlisted
// together.
func (s *Store) Signup(ctx context.Context, rsvpID int64, name string, guestCount int, now time.Time, loc *time.Location, enforceDeadline bool) (*Outcome, error) {
	var out Outcome
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		e, sess, err := lockedEntry(ctx, tx, rsvpID)
		if err != nil {
			return err
		}
		entries, err := entriesFor(ctx, tx, sess.ID)
		if err != nil {
			return err
		}
		roster := model.BuildRoster(*sess, entries)
		if enforceDeadline && !roster.SignupsOpen(now, loc) {
			return ErrSignupsClosed
		}

		// Exclude this person's own current slots so re-confirming an existing
		// RSVP is not blocked by the spots they already hold.
		roster = excludeSelf(roster, *sess, entries, e.ID)

		partySize := 1 + guestCount
		status, position := model.StatusConfirmed, 0
		if !roster.Fits(partySize) {
			status = model.StatusWaitlist
			position = roster.NextWaitlistPosition()
		}

		if name != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE player SET name = ?, updated_at = unixepoch() WHERE id = ?`, name, e.PlayerID); err != nil {
				return err
			}
		}
		var pos any
		if status == model.StatusWaitlist {
			pos = position
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rsvp SET status = ?, guest_count = ?, waitlist_position = ?, updated_at = unixepoch()
			 WHERE id = ?`, status, guestCount, pos, e.ID); err != nil {
			return err
		}

		updated, err := entryByIDTx(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		out = Outcome{Entry: *updated, Waitlisted: status == model.StatusWaitlist, Position: position}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetGuestCount adds or removes a guest on an existing RSVP. A confirmed
// participant may only add a guest if there is room; the alternative is
// silently splitting their party, which nobody wants explained over email.
func (s *Store) SetGuestCount(ctx context.Context, rsvpID int64, guestCount int, now time.Time, loc *time.Location) (*Outcome, error) {
	var out Outcome
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		e, sess, err := lockedEntry(ctx, tx, rsvpID)
		if err != nil {
			return err
		}
		entries, err := entriesFor(ctx, tx, sess.ID)
		if err != nil {
			return err
		}
		roster := model.BuildRoster(*sess, entries)
		if !roster.SignupsOpen(now, loc) {
			return ErrSignupsClosed
		}
		if e.Status == model.StatusConfirmed {
			free := excludeSelf(roster, *sess, entries, e.ID)
			if !free.Fits(1 + guestCount) {
				return ErrNoRoom
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rsvp SET guest_count = ?, updated_at = unixepoch() WHERE id = ?`, guestCount, e.ID); err != nil {
			return err
		}
		updated, err := entryByIDTx(ctx, tx, e.ID)
		if err != nil {
			return err
		}
		out = Outcome{Entry: *updated, Waitlisted: updated.Status == model.StatusWaitlist}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel releases an RSVP's slots. It deliberately does not promote anyone:
// the organizer decides who comes off the waitlist.
func (s *Store) Cancel(ctx context.Context, rsvpID int64) (*model.Entry, error) {
	var out *model.Entry
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE rsvp SET status = ?, waitlist_position = NULL, updated_at = unixepoch() WHERE id = ?`,
			model.StatusCancelled, rsvpID); err != nil {
			return err
		}
		e, err := entryByIDTx(ctx, tx, rsvpID)
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	return out, err
}

// PromoteNext moves the first waitlist entry that fits straight to confirmed
// and returns it, along with any entries ahead of it that were too large to
// fit. Returning the skipped list lets the UI explain why the queue was not
// followed in order instead of silently reordering people.
func (s *Store) PromoteNext(ctx context.Context, sessionID int64) (*model.Entry, []model.Entry, error) {
	var promoted *model.Entry
	var skipped []model.Entry
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		sess, err := sessionTx(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		entries, err := entriesFor(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		roster := model.BuildRoster(*sess, entries)
		candidate, skip := roster.NextPromotable()
		skipped = skip
		if candidate == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE rsvp SET status = ?, waitlist_position = NULL, updated_at = unixepoch() WHERE id = ?`,
			model.StatusConfirmed, candidate.ID); err != nil {
			return err
		}
		promoted, err = entryByIDTx(ctx, tx, candidate.ID)
		return err
	})
	return promoted, skipped, err
}

// EnsureEntry returns the RSVP row for a player in a session, creating an
// invited one with a fresh token if they do not have one. This is how somebody
// arriving through the public link, or added by hand, gets a personal link.
func (s *Store) EnsureEntry(ctx context.Context, sessionID, playerID int64, source string) (*model.Entry, error) {
	var rsvpID int64
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM rsvp WHERE session_id = ? AND player_id = ?`, sessionID, playerID).Scan(&rsvpID)
		if err == sql.ErrNoRows {
			res, err := tx.ExecContext(ctx,
				`INSERT INTO rsvp (session_id, player_id, status, guest_count, token, source, created_at, updated_at)
				 VALUES (?, ?, ?, 0, ?, ?, unixepoch(), unixepoch())`,
				sessionID, playerID, model.StatusInvited, NewToken(), source)
			if err != nil {
				return err
			}
			rsvpID, _ = res.LastInsertId()
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.EntryByID(ctx, rsvpID)
}

// EntryByID loads one RSVP with its player.
func (s *Store) EntryByID(ctx context.Context, id int64) (*model.Entry, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx,
		`SELECT `+rsvpCols+` FROM rsvp r JOIN player p ON p.id = r.player_id WHERE r.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// AddParticipant creates or revives an RSVP for a player the organizer adds by
// hand. This is the escape hatch for the person who texts instead of clicking;
// without it the organizer keeps a spreadsheet on the side.
func (s *Store) AddParticipant(ctx context.Context, sessionID, playerID int64, guestCount int) (*Outcome, error) {
	entry, err := s.EnsureEntry(ctx, sessionID, playerID, model.SourceManual)
	if err != nil {
		return nil, err
	}
	// The organizer can add people after the deadline, so it is not enforced here.
	return s.Signup(ctx, entry.ID, "", guestCount, time.Now(), time.UTC, false)
}

// PlayerEntry returns a player's RSVP for a session, if any.
func (s *Store) PlayerEntry(ctx context.Context, sessionID, playerID int64) (*model.Entry, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx,
		`SELECT `+rsvpCols+` FROM rsvp r JOIN player p ON p.id = r.player_id
		 WHERE r.session_id = ? AND r.player_id = ?`, sessionID, playerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// excludeSelf rebuilds a roster with one RSVP's slots released, so a change to
// that RSVP is not blocked by the capacity it already occupies.
func excludeSelf(r model.Roster, sess model.Session, entries []model.Entry, rsvpID int64) model.Roster {
	filtered := make([]model.Entry, 0, len(entries))
	for _, e := range entries {
		if e.ID != rsvpID {
			filtered = append(filtered, e)
		}
	}
	return model.BuildRoster(sess, filtered)
}

func lockedEntry(ctx context.Context, tx *sql.Tx, rsvpID int64) (*model.Entry, *model.Session, error) {
	e, err := entryByIDTx(ctx, tx, rsvpID)
	if err != nil {
		return nil, nil, err
	}
	sess, err := sessionTx(ctx, tx, e.SessionID)
	if err != nil {
		return nil, nil, err
	}
	return e, sess, nil
}

func entryByIDTx(ctx context.Context, tx *sql.Tx, id int64) (*model.Entry, error) {
	e, err := scanEntry(tx.QueryRowContext(ctx,
		`SELECT `+rsvpCols+` FROM rsvp r JOIN player p ON p.id = r.player_id WHERE r.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func sessionTx(ctx context.Context, tx *sql.Tx, id int64) (*model.Session, error) {
	sess, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM session WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}
