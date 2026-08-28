package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"badminton/internal/model"
)

// ErrDuplicateEmail is returned when an email already belongs to another player.
var ErrDuplicateEmail = errors.New("a player with that email already exists")

const playerCols = `id, name, email, active, token, unsubscribed_at, created_at, updated_at`

func scanPlayer(sc interface{ Scan(...any) error }) (model.Player, error) {
	var p model.Player
	var unsub sql.NullInt64
	err := sc.Scan(&p.ID, &p.Name, &p.Email, &p.Active, &p.Token, &unsub, &p.CreatedAt, &p.UpdatedAt)
	if unsub.Valid {
		v := unsub.Int64
		p.UnsubscribedAt = &v
	}
	return p, err
}

// Players returns the mailing list, optionally filtered by a search term over
// name and email.
func (s *Store) Players(ctx context.Context, search string) ([]model.Player, error) {
	q := `SELECT ` + playerCols + ` FROM player`
	args := []any{}
	if search = strings.TrimSpace(search); search != "" {
		q += ` WHERE name LIKE ? OR email LIKE ?`
		like := "%" + search + "%"
		args = append(args, like, like)
	}
	q += ` ORDER BY active DESC, name COLLATE NOCASE`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Player
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MailablePlayers returns everyone who should receive group email: active and
// not unsubscribed.
func (s *Store) MailablePlayers(ctx context.Context) ([]model.Player, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+playerCols+` FROM player
		 WHERE active = 1 AND unsubscribed_at IS NULL
		 ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Player
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Player looks up one player by id.
func (s *Store) Player(ctx context.Context, id int64) (*model.Player, error) {
	p, err := scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerCols+` FROM player WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PlayerByToken looks up a player by their unsubscribe token.
func (s *Store) PlayerByToken(ctx context.Context, token string) (*model.Player, error) {
	p, err := scanPlayer(s.db.QueryRowContext(ctx, `SELECT `+playerCols+` FROM player WHERE token = ?`, token))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PlayerByEmail looks up a player by email, case-insensitively.
func (s *Store) PlayerByEmail(ctx context.Context, email string) (*model.Player, error) {
	p, err := scanPlayer(s.db.QueryRowContext(ctx,
		`SELECT `+playerCols+` FROM player WHERE email = ? COLLATE NOCASE`, strings.TrimSpace(email)))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePlayer adds someone to the database. Players created by a public
// signup are added inactive so they do not silently join the mailing list.
func (s *Store) CreatePlayer(ctx context.Context, name, email string, active bool) (*model.Player, error) {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO player (name, email, active, token, created_at, updated_at)
		 VALUES (?, ?, ?, ?, unixepoch(), unixepoch())`, name, email, active, NewToken())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateEmail
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Player(ctx, id)
}

// UpdatePlayer edits a player's name, email and mailing-list membership.
func (s *Store) UpdatePlayer(ctx context.Context, id int64, name, email string, active bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE player SET name = ?, email = ?, active = ?, updated_at = unixepoch() WHERE id = ?`,
		strings.TrimSpace(name), strings.TrimSpace(email), active, id)
	if isUniqueViolation(err) {
		return ErrDuplicateEmail
	}
	return err
}

// DeletePlayer removes a player and, by cascade, their RSVPs.
func (s *Store) DeletePlayer(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM player WHERE id = ?`, id)
	return err
}

// SetUnsubscribed records or clears an unsubscribe.
func (s *Store) SetUnsubscribed(ctx context.Context, id int64, unsubscribed bool) error {
	var err error
	if unsubscribed {
		_, err = s.db.ExecContext(ctx,
			`UPDATE player SET unsubscribed_at = unixepoch(), updated_at = unixepoch() WHERE id = ?`, id)
	} else {
		_, err = s.db.ExecContext(ctx,
			`UPDATE player SET unsubscribed_at = NULL, updated_at = unixepoch() WHERE id = ?`, id)
	}
	return err
}

// ImportResult summarises a CSV import.
type ImportResult struct {
	Added   int
	Updated int
	Skipped int
	Errors  []string
}

// ImportPlayers upserts a batch of name/email pairs, matching on email so
// re-importing the same list updates names instead of creating duplicates.
func (s *Store) ImportPlayers(ctx context.Context, people []struct{ Name, Email string }) (ImportResult, error) {
	var res ImportResult
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		for _, p := range people {
			name, email := strings.TrimSpace(p.Name), strings.TrimSpace(p.Email)
			if email == "" {
				res.Skipped++
				continue
			}
			if name == "" {
				name = email
			}
			var id int64
			var existingName string
			err := tx.QueryRowContext(ctx,
				`SELECT id, name FROM player WHERE email = ? COLLATE NOCASE`, email).Scan(&id, &existingName)
			switch {
			case err == sql.ErrNoRows:
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO player (name, email, active, token, created_at, updated_at)
					 VALUES (?, ?, 1, ?, unixepoch(), unixepoch())`, name, email, NewToken()); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", email, err))
					continue
				}
				res.Added++
			case err != nil:
				return err
			default:
				// Re-activate on re-import; that is what "they are on the list again" means.
				if _, err := tx.ExecContext(ctx,
					`UPDATE player SET name = ?, active = 1, updated_at = unixepoch() WHERE id = ?`,
					name, id); err != nil {
					return err
				}
				res.Updated++
			}
		}
		return nil
	})
	return res, err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// PlayerHasHistory reports whether a player ever answered an invitation. Used
// to decide between deactivating someone and deleting them outright, so past
// rosters are never silently rewritten.
func (s *Store) PlayerHasHistory(ctx context.Context, playerID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rsvp WHERE player_id = ? AND status != 'invited'`, playerID).Scan(&n)
	return n > 0, err
}
