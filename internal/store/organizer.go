package store

import (
	"context"
	"database/sql"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Organizer is the single admin account.
type Organizer struct {
	ID           int64
	Name         string
	Email        string
	PasswordHash string
}

// EnsureOrganizer creates the organizer row on first boot from the configured
// name, email and password. On later boots it refreshes the name and email but
// leaves the password alone, so the env var can be rotated out of the
// deployment without locking anyone out or resetting a changed password.
func (s *Store) EnsureOrganizer(ctx context.Context, name, email, password string) (*Organizer, error) {
	existing, err := s.Organizer(ctx)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Name != name || existing.Email != email {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE organizer SET name = ?, email = ?, updated_at = unixepoch() WHERE id = ?`,
				name, email, existing.ID); err != nil {
				return nil, err
			}
			existing.Name, existing.Email = name, email
		}
		return existing, nil
	}

	if password == "" {
		return nil, fmt.Errorf("ORGANIZER_PASSWORD is required on first boot to create the organizer account")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO organizer (name, email, password_hash, created_at, updated_at)
		 VALUES (?, ?, ?, unixepoch(), unixepoch())`, name, email, string(hash))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Organizer{ID: id, Name: name, Email: email, PasswordHash: string(hash)}, nil
}

// Organizer returns the admin account, or nil if it has not been created yet.
func (s *Store) Organizer(ctx context.Context) (*Organizer, error) {
	var o Organizer
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, email, password_hash FROM organizer ORDER BY id LIMIT 1`).
		Scan(&o.ID, &o.Name, &o.Email, &o.PasswordHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CheckPassword verifies a login attempt in constant time.
func (o *Organizer) CheckPassword(password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(o.PasswordHash), []byte(password)) == nil
}

// SetOrganizerPassword replaces the stored password hash.
func (s *Store) SetOrganizerPassword(ctx context.Context, id int64, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE organizer SET password_hash = ?, updated_at = unixepoch() WHERE id = ?`, string(hash), id)
	return err
}
