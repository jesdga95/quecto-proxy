package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrKeyNotFound = errors.New("key not found")

type APIKey struct {
	ID              int64
	Name            string
	KeyHash         []byte
	KeyPrefix       string
	CreatedAt       int64
	ExpiresAt       *int64
	RevokedAt       *int64
	RPMLimit        *int
	TPMLimit        *int
	TotalTokenLimit *int64
	LastUsedAt      *int64

	TotalRequests         int64
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	TokensSpent           int64
}

// Status reports whether the key may be used, and why not when it may not.
// A spent quota is terminal like the other two: it does not come back on its
// own, so callers are told the key is finished rather than asked to retry.
func (k *APIKey) Status(now int64) (ok bool, reason string) {
	if k.RevokedAt != nil {
		return false, "revoked"
	}
	if k.ExpiresAt != nil && *k.ExpiresAt <= now {
		return false, "expired"
	}
	if k.TotalTokenLimit != nil && k.TokensSpent >= *k.TotalTokenLimit {
		return false, "exhausted"
	}
	return true, ""
}

const keyColumns = `id, name, key_hash, key_prefix, created_at, expires_at, revoked_at,
	rpm_limit, tpm_limit, total_token_limit, last_used_at,
	total_requests, total_prompt_tokens, total_completion_tokens, tokens_spent`

func scanKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var (
		k          APIKey
		expires    sql.NullInt64
		revoked    sql.NullInt64
		rpm, tpm   sql.NullInt64
		total      sql.NullInt64
		lastUsedAt sql.NullInt64
	)
	err := row.Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.KeyPrefix, &k.CreatedAt, &expires, &revoked,
		&rpm, &tpm, &total, &lastUsedAt,
		&k.TotalRequests, &k.TotalPromptTokens, &k.TotalCompletionTokens, &k.TokensSpent,
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
	if total.Valid {
		k.TotalTokenLimit = &total.Int64
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
	Name            string
	KeyHash         []byte
	KeyPrefix       string
	CreatedAt       int64
	ExpiresAt       *int64
	RPMLimit        *int
	TPMLimit        *int
	TotalTokenLimit *int64
}

func (s *Store) CreateKey(n NewKey) (*APIKey, error) {
	res, err := s.DB.Exec(
		`INSERT INTO api_keys (name, key_hash, key_prefix, created_at, expires_at, rpm_limit, tpm_limit, total_token_limit)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Name, n.KeyHash, n.KeyPrefix, n.CreatedAt, nullInt64(n.ExpiresAt),
		nullInt(n.RPMLimit), nullInt(n.TPMLimit), nullInt64(n.TotalTokenLimit),
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
	Name            *string
	ExpiresAt       **int64
	RPMLimit        **int
	TPMLimit        **int
	TotalTokenLimit **int64
	// Set to clear the lifetime tally, which revives a key that spent its quota
	// without disturbing the usage history the dashboard reports.
	ResetTokensSpent bool
}

func (s *Store) UpdateKey(id int64, u KeyUpdate) (*APIKey, error) {
	sets := make([]string, 0, 6)
	args := make([]any, 0, 7)
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
	if u.TotalTokenLimit != nil {
		sets = append(sets, "total_token_limit = ?")
		args = append(args, nullInt64(*u.TotalTokenLimit))
	}
	if u.ResetTokensSpent {
		sets = append(sets, "tokens_spent = 0")
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

// DeletedKeyID holds the rollup of keys that no longer exist, so their traffic
// stays in the dashboard totals. SQLite never hands out id 0, so no live key
// can collide with it.
const DeletedKeyID = 0

func (s *Store) DeleteKey(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("delete key %d: %w", id, err)
	}
	defer tx.Rollback()

	// The rollup carries no foreign key, and SQLite hands the freed id to the
	// next key, so park the rows on the sentinel or the successor inherits them.
	if _, err := tx.Exec(`INSERT INTO usage_hourly
		(bucket, key_id, model, requests, errors, prompt_tokens, completion_tokens, cached_tokens, cache_known_prompt_tokens)
		SELECT bucket, ?, model, requests, errors, prompt_tokens, completion_tokens, cached_tokens, cache_known_prompt_tokens
		FROM usage_hourly WHERE key_id = ?
		ON CONFLICT(bucket, key_id, model) DO UPDATE SET
			requests = requests + excluded.requests,
			errors = errors + excluded.errors,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens,
			cached_tokens = cached_tokens + excluded.cached_tokens,
			cache_known_prompt_tokens = cache_known_prompt_tokens + excluded.cache_known_prompt_tokens`,
		DeletedKeyID, id); err != nil {
		return fmt.Errorf("delete key %d: %w", id, err)
	}
	if _, err := tx.Exec("DELETE FROM usage_hourly WHERE key_id = ?", id); err != nil {
		return fmt.Errorf("delete key %d: %w", id, err)
	}
	res, err := tx.Exec("DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete key %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotFound
	}
	return tx.Commit()
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
