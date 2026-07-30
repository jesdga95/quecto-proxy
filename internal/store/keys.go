package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrKeyNotFound = errors.New("key not found")

type APIKey struct {
	ID         int64
	Name       string
	KeyHash    []byte
	KeyPrefix  string
	CreatedAt  int64
	ExpiresAt  *int64
	RevokedAt  *int64
	RPMLimit   *int
	TPMLimit   *int
	LastUsedAt *int64

	TotalRequests         int64
	TotalPromptTokens     int64
	TotalCompletionTokens int64
}

// Status reports whether the key may be used, and why not when it may not.
func (k *APIKey) Status(now int64) (ok bool, reason string) {
	if k.RevokedAt != nil {
		return false, "revoked"
	}
	if k.ExpiresAt != nil && *k.ExpiresAt <= now {
		return false, "expired"
	}
	return true, ""
}

const keyColumns = `id, name, key_hash, key_prefix, created_at, expires_at, revoked_at,
	rpm_limit, tpm_limit, last_used_at,
	total_requests, total_prompt_tokens, total_completion_tokens`

func scanKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var (
		k          APIKey
		expires    sql.NullInt64
		revoked    sql.NullInt64
		rpm, tpm   sql.NullInt64
		lastUsedAt sql.NullInt64
	)
	err := row.Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.CreatedAt, &expires, &revoked,
		&rpm, &tpm, &lastUsedAt,
		&k.TotalRequests, &k.TotalPromptTokens, &k.TotalCompletionTokens,
	)
	if err != nil {
		return nil, err
	}
	if expires.Valid {
		k.ExpiresAt = &expires.Int64
	}
	if revoked.Valid {
		k.RevokedAt = &revoked.Int64
	}
	if rpm.Valid {
		n := int(rpm.Int64)
		k.RPMLimit = &n
	}
	if tpm.Valid {
		n := int(tpm.Int64)
		k.TPMLimit = &n
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = &lastUsedAt.Int64
	}
	return &k, nil
}

func (s *Store) ListKeys() ([]*APIKey, error) {
	rows, err := s.DB.Query("SELECT " + keyColumns + " FROM api_keys ORDER BY created_at DESC")
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) GetKey(id int64) (*APIKey, error) {
	k, err := scanKey(s.DB.QueryRow("SELECT "+keyColumns+" FROM api_keys WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get key %d: %w", id, err)
	}
	return k, nil
}

type NewKey struct {
	Name      string
	KeyHash   []byte
	KeyPrefix string
	CreatedAt int64
	ExpiresAt *int64
	RPMLimit  *int
	TPMLimit  *int
}

func (s *Store) CreateKey(n NewKey) (*APIKey, error) {
	res, err := s.DB.Exec(
		`INSERT INTO api_keys (name, key_hash, key_prefix, created_at, expires_at, rpm_limit, tpm_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.Name, n.KeyHash, n.KeyPrefix, n.CreatedAt, nullInt64(n.ExpiresAt), nullInt(n.RPMLimit), nullInt(n.TPMLimit),
	)
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	return s.GetKey(id)
}

type KeyUpdate struct {
	Name      *string
	ExpiresAt **int64
	RPMLimit  **int
	TPMLimit  **int
}

func (s *Store) UpdateKey(id int64, u KeyUpdate) (*APIKey, error) {
	sets := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if u.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *u.Name)
	}
	if u.ExpiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, nullInt64(*u.ExpiresAt))
	}
	if u.RPMLimit != nil {
		sets = append(sets, "rpm_limit = ?")
		args = append(args, nullInt(*u.RPMLimit))
	}
	if u.TPMLimit != nil {
		sets = append(sets, "tpm_limit = ?")
		args = append(args, nullInt(*u.TPMLimit))
	}
	if len(sets) == 0 {
		return s.GetKey(id)
	}
	args = append(args, id)

	res, err := s.DB.Exec("UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return nil, fmt.Errorf("update key %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrKeyNotFound
	}
	return s.GetKey(id)
}

func (s *Store) RevokeKey(id int64, at int64) error {
	res, err := s.DB.Exec("UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", at, id)
	if err != nil {
		return fmt.Errorf("revoke key %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetKey(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteKey(id int64) error {
	res, err := s.DB.Exec("DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete key %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

type UsageDelta struct {
	KeyID            int64
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	LastUsedAt       int64
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
