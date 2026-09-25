package store

import (
	"database/sql"
	"fmt"
	"strings"
)

type LogRow struct {
	KeyID            int64
	TS               int64
	Model            string
	Status           int
	Stream           bool
	PromptTokens     *int64
	CompletionTokens *int64
	DurationMS       int64
	TTFTMS           *int64
}

type hourlyKey struct {
	Bucket int64
	KeyID  int64
	Model  string
}

type hourlyDelta struct {
	Requests         int64
	Errors           int64
	PromptTokens     int64
	CompletionTokens int64
}

// WriteBatch appends log rows and folds them into the hourly rollup and the
// per-key counters, all in one transaction so there is a single fsync.
func (s *Store) WriteBatch(rows []LogRow) error {
	if len(rows) == 0 {
		return nil
	}

	// Fold in memory first so a burst becomes a few statements, not hundreds.
	hourly := make(map[hourlyKey]*hourlyDelta, len(rows))
	perKey := make(map[int64]*UsageDelta, 8)
	for _, r := range rows {
		hk := hourlyKey{Bucket: r.TS - r.TS%3600, KeyID: r.KeyID, Model: r.Model}
		h := hourly[hk]
		if h == nil {
			h = &hourlyDelta{}
			hourly[hk] = h
		}
		h.Requests++
		if r.Status >= 400 {
			h.Errors++
		}

		d := perKey[r.KeyID]
		if d == nil {
			d = &UsageDelta{KeyID: r.KeyID}
			perKey[r.KeyID] = d
		}
		d.Requests++
		if r.TS > d.LastUsedAt {
			d.LastUsedAt = r.TS
		}
		if r.PromptTokens != nil {
			h.PromptTokens += *r.PromptTokens
			d.PromptTokens += *r.PromptTokens
		}
		if r.CompletionTokens != nil {
			h.CompletionTokens += *r.CompletionTokens
			d.CompletionTokens += *r.CompletionTokens
		}
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("write usage batch: %w", err)
	}
	defer tx.Rollback()

	insert, err := tx.Prepare(`INSERT INTO request_log
		(key_id, ts, model, status, stream, prompt_tokens, completion_tokens, duration_ms, ttft_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("write usage batch: %w", err)
	}
	defer insert.Close()

	for _, r := range rows {
		if _, err := insert.Exec(r.KeyID, r.TS, r.Model, r.Status, boolInt(r.Stream),
			nullInt64(r.PromptTokens), nullInt64(r.CompletionTokens), r.DurationMS, nullInt64(r.TTFTMS)); err != nil {
			return fmt.Errorf("write usage batch: %w", err)
		}
	}

	roll, err := tx.Prepare(`INSERT INTO usage_hourly
		(bucket, key_id, model, requests, errors, prompt_tokens, completion_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, key_id, model) DO UPDATE SET
			requests = requests + excluded.requests,
			errors = errors + excluded.errors,
			prompt_tokens = prompt_tokens + excluded.prompt_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens`)
	if err != nil {
		return fmt.Errorf("write usage batch: %w", err)
	}
	defer roll.Close()

	for k, v := range hourly {
		if _, err := roll.Exec(k.Bucket, k.KeyID, k.Model, v.Requests, v.Errors, v.PromptTokens, v.CompletionTokens); err != nil {
			return fmt.Errorf("write usage batch: %w", err)
		}
	}

	// tokens_spent rides along here so the lifetime quota survives a restart.
	// The limiter holds the authoritative tally while the process runs and seeds
	// itself from this column on the way up.
	counters, err := tx.Prepare(`UPDATE api_keys SET
		total_requests = total_requests + ?,
		total_prompt_tokens = total_prompt_tokens + ?,
		total_completion_tokens = total_completion_tokens + ?,
		tokens_spent = tokens_spent + ?,
		last_used_at = MAX(COALESCE(last_used_at, 0), ?)
		WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("write usage batch: %w", err)
	}
	defer counters.Close()

	for _, d := range perKey {
		if _, err := counters.Exec(d.Requests, d.PromptTokens, d.CompletionTokens,
			d.PromptTokens+d.CompletionTokens, d.LastUsedAt, d.KeyID); err != nil {
			return fmt.Errorf("write usage batch: %w", err)
		}
	}
	return tx.Commit()
}

// Prune enforces both the age limit and the row ceiling.
func (s *Store) Prune(olderThan int64, maxRows int64) (int64, error) {
	var total int64

	res, err := s.DB.Exec("DELETE FROM request_log WHERE ts < ?", olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune by age: %w", err)
	}
	n, _ := res.RowsAffected()
	total += n

	var cutoff sql.NullInt64
	err = s.DB.QueryRow("SELECT id FROM request_log ORDER BY id DESC LIMIT 1 OFFSET ?", maxRows).Scan(&cutoff)
	if err != nil && err != sql.ErrNoRows {
		return total, fmt.Errorf("prune by count: %w", err)
	}
	if cutoff.Valid {
		res, err := s.DB.Exec("DELETE FROM request_log WHERE id <= ?", cutoff.Int64)
		if err != nil {
			return total, fmt.Errorf("prune by count: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// PruneRollup drops hourly rows past a far longer horizon than the raw log.
func (s *Store) PruneRollup(olderThan int64) (int64, error) {
	res, err := s.DB.Exec("DELETE FROM usage_hourly WHERE bucket < ?", olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune rollup: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

type LogFilter struct {
	KeyID  *int64
	Status string
	From   *int64
	To     *int64
	Cursor *int64
	Limit  int
}

type LogEntry struct {
	ID               int64  `json:"id"`
	KeyID            int64  `json:"key_id"`
	KeyName          string `json:"key_name"`
	TS               int64  `json:"ts"`
	Model            string `json:"model"`
	Status           int    `json:"status"`
	Stream           bool   `json:"stream"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	DurationMS       int64  `json:"duration_ms"`
	TTFTMS           *int64 `json:"ttft_ms"`
}

func (s *Store) ListLogs(f LogFilter) ([]LogEntry, *int64, error) {
	where := []string{}
	args := []any{}
	if f.KeyID != nil {
		where = append(where, "l.key_id = ?")
		args = append(args, *f.KeyID)
	}
	switch f.Status {
	case "success":
		where = append(where, "l.status < 400")
	case "rate_limited":
		where = append(where, "l.status = 429")
	case "client_error":
		where = append(where, "l.status >= 400 AND l.status < 500")
	case "server_error":
		where = append(where, "l.status >= 500")
	}
	if f.From != nil {
		where = append(where, "l.ts >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		where = append(where, "l.ts <= ?")
		args = append(args, *f.To)
	}
	if f.Cursor != nil {
		where = append(where, "l.id < ?")
		args = append(args, *f.Cursor)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}

	q := `SELECT l.id, l.key_id, COALESCE(k.name, ''), l.ts, COALESCE(l.model, ''),
		l.status, l.stream, l.prompt_tokens, l.completion_tokens, l.duration_ms, l.ttft_ms
		FROM request_log l LEFT JOIN api_keys k ON k.id = l.key_id`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// One extra row tells us whether another page exists.
	q += " ORDER BY l.id DESC LIMIT ?"
	args = append(args, f.Limit+1)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list logs: %w", err)
	}
	defer rows.Close()

	out := make([]LogEntry, 0, f.Limit)
	for rows.Next() {
		var e LogEntry
		var stream int
		var pt, ct, ttft sql.NullInt64
		if err := rows.Scan(&e.ID, &e.KeyID, &e.KeyName, &e.TS, &e.Model,
			&e.Status, &stream, &pt, &ct, &e.DurationMS, &ttft); err != nil {
			return nil, nil, fmt.Errorf("list logs: %w", err)
		}
		e.Stream = stream != 0
		if pt.Valid {
			e.PromptTokens = &pt.Int64
		}
		if ct.Valid {
			e.CompletionTokens = &ct.Int64
		}
		if ttft.Valid {
			e.TTFTMS = &ttft.Int64
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list logs: %w", err)
	}

	var next *int64
	if len(out) > f.Limit {
		out = out[:f.Limit]
		id := out[len(out)-1].ID
		next = &id
	}
	return out, next, nil
}

type Totals struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type Bucket struct {
	Bucket   int64 `json:"bucket"`
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

type KeyTotal struct {
	KeyID    int64  `json:"key_id"`
	KeyName  string `json:"key_name"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// Totals reads the rollup, so it stays correct after raw rows are pruned.
func (s *Store) Totals(from, to int64) (Totals, error) {
	var t Totals
	err := s.DB.QueryRow(`SELECT
		COALESCE(SUM(requests), 0), COALESCE(SUM(errors), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM usage_hourly WHERE bucket >= ? AND bucket <= ?`, from, to).
		Scan(&t.Requests, &t.Errors, &t.PromptTokens, &t.CompletionTokens)
	if err != nil {
		return t, fmt.Errorf("read totals: %w", err)
	}
	return t, nil
}

func (s *Store) Series(from, to int64) ([]Bucket, error) {
	rows, err := s.DB.Query(`SELECT bucket,
		SUM(requests), SUM(errors), SUM(prompt_tokens) + SUM(completion_tokens)
		FROM usage_hourly WHERE bucket >= ? AND bucket <= ?
		GROUP BY bucket ORDER BY bucket`, from, to)
	if err != nil {
		return nil, fmt.Errorf("read series: %w", err)
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.Errors, &b.Tokens); err != nil {
			return nil, fmt.Errorf("read series: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecentTotals reads the raw log rather than the rollup, because the rollup
// only has hourly buckets and a partial hour would overcount a short window.
func (s *Store) RecentTotals(from, to int64) (Totals, error) {
	var t Totals
	err := s.DB.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM request_log WHERE ts >= ? AND ts <= ?`, from, to).
		Scan(&t.Requests, &t.Errors, &t.PromptTokens, &t.CompletionTokens)
	if err != nil {
		return t, fmt.Errorf("read recent totals: %w", err)
	}
	return t, nil
}

// RecentSeries buckets the raw log at a step finer than the hourly rollup, so
// a short window still has shape on the chart.
func (s *Store) RecentSeries(from, to, step int64) ([]Bucket, error) {
	rows, err := s.DB.Query(`SELECT ts - ts % ?, COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0) + COALESCE(SUM(completion_tokens), 0)
		FROM request_log WHERE ts >= ? AND ts <= ?
		GROUP BY ts - ts % ? ORDER BY ts - ts % ?`, step, from, to, step, step)
	if err != nil {
		return nil, fmt.Errorf("read recent series: %w", err)
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.Errors, &b.Tokens); err != nil {
			return nil, fmt.Errorf("read recent series: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// RecentTopKeys ranks keys from the raw log. A key that is already gone has
// no row in api_keys, so it folds into the same sentinel row as the rollup.
func (s *Store) RecentTopKeys(from, to int64, limit int) ([]KeyTotal, error) {
	rows, err := s.DB.Query(`SELECT CASE WHEN k.id IS NULL THEN ? ELSE l.key_id END AS key_id,
		COALESCE(k.name, ''),
		COUNT(*),
		COALESCE(SUM(l.prompt_tokens), 0) + COALESCE(SUM(l.completion_tokens), 0) AS tokens
		FROM request_log l LEFT JOIN api_keys k ON k.id = l.key_id
		WHERE l.ts >= ? AND l.ts <= ?
		GROUP BY key_id ORDER BY tokens DESC LIMIT ?`, DeletedKeyID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("read recent top keys: %w", err)
	}
	defer rows.Close()

	var out []KeyTotal
	for rows.Next() {
		var k KeyTotal
		if err := rows.Scan(&k.KeyID, &k.KeyName, &k.Requests, &k.Tokens); err != nil {
			return nil, fmt.Errorf("read recent top keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TopKeys ranks keys, not key and model pairs, so a key that serves more than
// one model is still a single row.
func (s *Store) TopKeys(from, to int64, limit int) ([]KeyTotal, error) {
	rows, err := s.DB.Query(`SELECT u.key_id, COALESCE(k.name, ''),
		SUM(u.requests), SUM(u.prompt_tokens) + SUM(u.completion_tokens) AS tokens
		FROM usage_hourly u LEFT JOIN api_keys k ON k.id = u.key_id
		WHERE u.bucket >= ? AND u.bucket <= ?
		GROUP BY u.key_id ORDER BY tokens DESC LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("read top keys: %w", err)
	}
	defer rows.Close()

	var out []KeyTotal
	for rows.Next() {
		var k KeyTotal
		if err := rows.Scan(&k.KeyID, &k.KeyName, &k.Requests, &k.Tokens); err != nil {
			return nil, fmt.Errorf("read top keys: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Percentile reads one column of the raw log, which only covers the retention window.
func (s *Store) Percentile(column string, from, to int64, p float64) (*int64, error) {
	switch column {
	case "ttft_ms", "duration_ms":
	default:
		return nil, fmt.Errorf("percentile is not available for %q", column)
	}

	var count int64
	err := s.DB.QueryRow(
		"SELECT COUNT(*) FROM request_log WHERE "+column+" IS NOT NULL AND ts >= ? AND ts <= ?",
		from, to).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("read percentile: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	offset := int64(float64(count-1) * p)

	var v int64
	err = s.DB.QueryRow(
		"SELECT "+column+" FROM request_log WHERE "+column+" IS NOT NULL AND ts >= ? AND ts <= ?"+
			" ORDER BY "+column+" LIMIT 1 OFFSET ?", from, to, offset).Scan(&v)
	if err != nil {
		return nil, fmt.Errorf("read percentile: %w", err)
	}
	return &v, nil
}

func (s *Store) LogStats() (rows int64, bytes int64, err error) {
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM request_log").Scan(&rows); err != nil {
		return 0, 0, fmt.Errorf("count log rows: %w", err)
	}
	var pageCount, pageSize int64
	if err = s.DB.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return rows, 0, nil
	}
	if err = s.DB.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return rows, 0, nil
	}
	return rows, pageCount * pageSize, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
