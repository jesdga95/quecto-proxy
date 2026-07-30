package store

import (
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) Setting(key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read setting %s: %w", key, err)
	}
	return v, true, nil
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.DB.Exec(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	if err != nil {
		return fmt.Errorf("write setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) LoadLimiterState() (map[int64]float64, error) {
	rows, err := s.DB.Query("SELECT key_id, tpm_balance FROM limiter_state")
	if err != nil {
		return nil, fmt.Errorf("read limiter state: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]float64)
	for rows.Next() {
		var id int64
		var balance float64
		if err := rows.Scan(&id, &balance); err != nil {
			return nil, fmt.Errorf("read limiter state: %w", err)
		}
		out[id] = balance
	}
	return out, rows.Err()
}

func (s *Store) SaveLimiterState(balances map[int64]float64, at int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("save limiter state: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM limiter_state"); err != nil {
		return fmt.Errorf("save limiter state: %w", err)
	}
	stmt, err := tx.Prepare("INSERT INTO limiter_state (key_id, tpm_balance, saved_at) VALUES (?, ?, ?)")
	if err != nil {
		return fmt.Errorf("save limiter state: %w", err)
	}
	defer stmt.Close()

	for id, balance := range balances {
		// A foreign key failure just means the key was deleted while running.
		if _, err := stmt.Exec(id, balance, at); err != nil {
			continue
		}
	}
	return tx.Commit()
}
