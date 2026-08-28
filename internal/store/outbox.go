package store

import (
	"context"
	"database/sql"

	"badminton/internal/model"
)

const outboxCols = `id, kind, session_id, player_id, to_email, to_name, subject,
	text_body, html_body, unsubscribe_url, status, attempts, next_attempt_at,
	last_error, created_at, sent_at`

func scanOutbox(sc interface{ Scan(...any) error }) (model.OutboxMessage, error) {
	var m model.OutboxMessage
	var sessID, playerID, sentAt sql.NullInt64
	err := sc.Scan(&m.ID, &m.Kind, &sessID, &playerID, &m.ToEmail, &m.ToName, &m.Subject,
		&m.TextBody, &m.HTMLBody, &m.UnsubscribeURL, &m.Status, &m.Attempts, &m.NextAttemptAt,
		&m.LastError, &m.CreatedAt, &sentAt)
	if sessID.Valid {
		v := sessID.Int64
		m.SessionID = &v
	}
	if playerID.Valid {
		v := playerID.Int64
		m.PlayerID = &v
	}
	if sentAt.Valid {
		v := sentAt.Int64
		m.SentAt = &v
	}
	return m, err
}

// Enqueue queues a message for the outbox worker. Sending never happens inline:
// 32 SMTP round trips would hang the request and lose everything on a failure
// halfway through.
func (s *Store) Enqueue(ctx context.Context, msgs ...model.OutboxMessage) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, m := range msgs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO email_outbox (kind, session_id, player_id, to_email, to_name,
					subject, text_body, html_body, unsubscribe_url, status, attempts,
					next_attempt_at, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, unixepoch(), unixepoch())`,
				m.Kind, m.SessionID, m.PlayerID, m.ToEmail, m.ToName, m.Subject,
				m.TextBody, m.HTMLBody, m.UnsubscribeURL, model.OutboxPending); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClaimDue returns pending messages whose retry time has arrived.
func (s *Store) ClaimDue(ctx context.Context, now int64, limit int) ([]model.OutboxMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+outboxCols+` FROM email_outbox
		 WHERE status = ? AND next_attempt_at <= ?
		 ORDER BY id LIMIT ?`, model.OutboxPending, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OutboxMessage
	for rows.Next() {
		m, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkSent records a successful delivery.
func (s *Store) MarkSent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_outbox SET status = ?, attempts = attempts + 1, sent_at = unixepoch(),
			last_error = '' WHERE id = ?`, model.OutboxSent, id)
	return err
}

// MarkRetry schedules another attempt after a transient failure.
func (s *Store) MarkRetry(ctx context.Context, id int64, nextAttemptAt int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?
		 WHERE id = ?`, nextAttemptAt, errMsg, id)
	return err
}

// MarkFailed gives up on a message after the attempt cap.
func (s *Store) MarkFailed(ctx context.Context, id int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_outbox SET status = ?, attempts = attempts + 1, last_error = ? WHERE id = ?`,
		model.OutboxFailed, errMsg, id)
	return err
}

// RetryMessage puts a failed message back in the queue.
func (s *Store) RetryMessage(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE email_outbox SET status = ?, attempts = 0, next_attempt_at = unixepoch(), last_error = ''
		 WHERE id = ? AND status = ?`, model.OutboxPending, id, model.OutboxFailed)
	return err
}

// RecentMessages returns the delivery log, newest first.
func (s *Store) RecentMessages(ctx context.Context, limit int) ([]model.OutboxMessage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+outboxCols+` FROM email_outbox ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OutboxMessage
	for rows.Next() {
		m, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// OutboxStats counts messages by delivery state.
type OutboxStats struct {
	Pending int
	Sent    int
	Failed  int
}

// SessionMailStats summarises delivery for one session, so the dashboard can
// show "32 sent, 1 failed" when someone says they never got the email.
func (s *Store) SessionMailStats(ctx context.Context, sessionID int64) (OutboxStats, error) {
	var st OutboxStats
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM email_outbox WHERE session_id = ? GROUP BY status`, sessionID)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		switch status {
		case model.OutboxPending:
			st.Pending = n
		case model.OutboxSent:
			st.Sent = n
		case model.OutboxFailed:
			st.Failed = n
		}
	}
	return st, rows.Err()
}

// PendingCount reports how many messages are still queued overall.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_outbox WHERE status = ?`, model.OutboxPending).Scan(&n)
	return n, err
}
