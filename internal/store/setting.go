package store

import (
	"context"
	"database/sql"
	"strconv"
)

// Defaults prefill the new-session form so the weekly job is a couple of clicks
// rather than six fields typed from scratch.
type Defaults struct {
	Courts                int
	CostPerCourtHourCents int64
	MaxPlayers            int
	StartTime             string
	EndTime               string
	DeadlineDaysBefore    int
	DeadlineTime          string
	ThingsToBring         string
}

// DefaultDefaults are used before the organizer has saved any settings.
func DefaultDefaults() Defaults {
	return Defaults{
		Courts:                3,
		CostPerCourtHourCents: 3500,
		MaxPlayers:            12,
		StartTime:             "19:00",
		EndTime:               "21:00",
		DeadlineDaysBefore:    3,
		DeadlineTime:          "15:00",
		ThingsToBring:         "Racquet, indoor court shoes, water. Shuttles are provided.",
	}
}

// Settings returns every stored setting as a map.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM setting`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Defaults loads the session defaults, falling back to sensible values for
// anything not yet configured.
func (s *Store) Defaults(ctx context.Context) (Defaults, error) {
	d := DefaultDefaults()
	m, err := s.Settings(ctx)
	if err != nil {
		return d, err
	}
	if v, ok := m["default_courts"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d.Courts = n
		}
	}
	if v, ok := m["default_cost_cents"]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			d.CostPerCourtHourCents = n
		}
	}
	if v, ok := m["default_max_players"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			d.MaxPlayers = n
		}
	}
	if v, ok := m["default_start_time"]; ok && v != "" {
		d.StartTime = v
	}
	if v, ok := m["default_end_time"]; ok && v != "" {
		d.EndTime = v
	}
	if v, ok := m["deadline_days_before"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			d.DeadlineDaysBefore = n
		}
	}
	if v, ok := m["deadline_time"]; ok && v != "" {
		d.DeadlineTime = v
	}
	if v, ok := m["things_to_bring"]; ok {
		d.ThingsToBring = v
	}
	return d, nil
}

// SaveDefaults persists the session defaults.
func (s *Store) SaveDefaults(ctx context.Context, d Defaults) error {
	return s.SetSettings(ctx, map[string]string{
		"default_courts":       strconv.Itoa(d.Courts),
		"default_cost_cents":   strconv.FormatInt(d.CostPerCourtHourCents, 10),
		"default_max_players":  strconv.Itoa(d.MaxPlayers),
		"default_start_time":   d.StartTime,
		"default_end_time":     d.EndTime,
		"deadline_days_before": strconv.Itoa(d.DeadlineDaysBefore),
		"deadline_time":        d.DeadlineTime,
		"things_to_bring":      d.ThingsToBring,
	})
}

// SetSettings upserts a batch of settings.
func (s *Store) SetSettings(ctx context.Context, kv map[string]string) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO setting (key, value) VALUES (?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return nil
	})
}
