package store

import (
	"context"
	"database/sql"

	"badminton/internal/model"
)

const sessionCols = `id, public_id, date, start_time, end_time, venue, courts,
	cost_per_court_hour_cents, max_players, signup_deadline_at, status, notes,
	invitation_sent_at, last_reminder_at, created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (model.Session, error) {
	var s model.Session
	var invited, reminded sql.NullInt64
	err := sc.Scan(&s.ID, &s.PublicID, &s.Date, &s.StartTime, &s.EndTime, &s.Venue, &s.Courts,
		&s.CostPerCourtHourCents, &s.MaxPlayers, &s.SignupDeadlineAt, &s.Status, &s.Notes,
		&invited, &reminded, &s.CreatedAt, &s.UpdatedAt)
	if invited.Valid {
		v := invited.Int64
		s.InvitationSentAt = &v
	}
	if reminded.Valid {
		v := reminded.Int64
		s.LastReminderAt = &v
	}
	return s, err
}

// CreateSession inserts a session and returns it with its generated id and
// public id.
func (s *Store) CreateSession(ctx context.Context, in model.Session) (*model.Session, error) {
	in.PublicID = NewToken()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO session (public_id, date, start_time, end_time, venue, courts,
			cost_per_court_hour_cents, max_players, signup_deadline_at, status, notes,
			created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch(), unixepoch())`,
		in.PublicID, in.Date, in.StartTime, in.EndTime, in.Venue, in.Courts,
		in.CostPerCourtHourCents, in.MaxPlayers, in.SignupDeadlineAt, in.Status, in.Notes)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Session(ctx, id)
}

// Session loads one session by id.
func (s *Store) Session(ctx context.Context, id int64) (*model.Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM session WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// SessionByPublicID loads the session behind a public signup link.
func (s *Store) SessionByPublicID(ctx context.Context, publicID string) (*model.Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM session WHERE public_id = ?`, publicID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// Sessions lists sessions ordered by date. Newest first when past is true,
// soonest first otherwise. The cutoff is a local date string so "today" stays
// in the upcoming list all day.
func (s *Store) Sessions(ctx context.Context, today string, past bool) ([]model.Session, error) {
	q := `SELECT ` + sessionCols + ` FROM session WHERE date >= ? ORDER BY date, start_time`
	if past {
		q = `SELECT ` + sessionCols + ` FROM session WHERE date < ? ORDER BY date DESC, start_time DESC`
	}
	rows, err := s.db.QueryContext(ctx, q, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// LatestSession returns the most recently created session, used to prefill
// "duplicate last session".
func (s *Store) LatestSession(ctx context.Context) (*model.Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM session ORDER BY date DESC, id DESC LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// UpdateSession saves edits to a session's details.
func (s *Store) UpdateSession(ctx context.Context, in model.Session) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET date = ?, start_time = ?, end_time = ?, venue = ?, courts = ?,
			cost_per_court_hour_cents = ?, max_players = ?, signup_deadline_at = ?,
			notes = ?, updated_at = unixepoch()
		 WHERE id = ?`,
		in.Date, in.StartTime, in.EndTime, in.Venue, in.Courts, in.CostPerCourtHourCents,
		in.MaxPlayers, in.SignupDeadlineAt, in.Notes, in.ID)
	return err
}

// SetSessionStatus changes the stored status (draft, open, closed, cancelled).
func (s *Store) SetSessionStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET status = ?, updated_at = unixepoch() WHERE id = ?`, status, id)
	return err
}

// SetCourts changes the court count, which is how the organizer records
// cancelling a court.
func (s *Store) SetCourts(ctx context.Context, id int64, courts int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET courts = ?, updated_at = unixepoch() WHERE id = ?`, courts, id)
	return err
}

// MarkInvitationSent records that the invitation went out, so the button can
// become a guarded "resend".
func (s *Store) MarkInvitationSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET invitation_sent_at = unixepoch(), updated_at = unixepoch() WHERE id = ?`, id)
	return err
}

// MarkReminderSent records the time of the last reminder.
func (s *Store) MarkReminderSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE session SET last_reminder_at = unixepoch(), updated_at = unixepoch() WHERE id = ?`, id)
	return err
}

// CourtDeadlines returns the per-court release deadlines for a session.
func (s *Store) CourtDeadlines(ctx context.Context, sessionID int64) ([]model.CourtDeadline, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, court_number, deadline_at FROM court_deadline
		 WHERE session_id = ? ORDER BY court_number`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CourtDeadline
	for rows.Next() {
		var d model.CourtDeadline
		if err := rows.Scan(&d.ID, &d.SessionID, &d.CourtNumber, &d.DeadlineAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReplaceCourtDeadlines swaps the whole set for a session.
func (s *Store) ReplaceCourtDeadlines(ctx context.Context, sessionID int64, ds []model.CourtDeadline) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM court_deadline WHERE session_id = ?`, sessionID); err != nil {
			return err
		}
		for _, d := range ds {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO court_deadline (session_id, court_number, deadline_at) VALUES (?, ?, ?)`,
				sessionID, d.CourtNumber, d.DeadlineAt); err != nil {
				return err
			}
		}
		return nil
	})
}
