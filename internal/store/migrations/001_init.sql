CREATE TABLE api_keys (
  id           INTEGER PRIMARY KEY,
  name         TEXT    NOT NULL,
  key_hash     BLOB    NOT NULL UNIQUE,
  key_prefix   TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  revoked_at   INTEGER,
  rpm_limit    INTEGER,
  tpm_limit    INTEGER,
  -- A lifetime allowance that never refills. Once tokens_spent reaches it the
  -- key is dead until the limit is raised or cleared.
  total_token_limit INTEGER,
  last_used_at INTEGER,
  total_requests          INTEGER NOT NULL DEFAULT 0,
  total_prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  total_completion_tokens INTEGER NOT NULL DEFAULT 0,
  -- Kept apart from the reporting totals so a quota can be reset without
  -- rewriting usage history.
  tokens_spent            INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE request_log (
  id                INTEGER PRIMARY KEY,
  key_id            INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  ts                INTEGER NOT NULL,
  model             TEXT,
  status            INTEGER NOT NULL,
  stream            INTEGER NOT NULL,
  prompt_tokens     INTEGER,
  completion_tokens INTEGER,
  duration_ms       INTEGER NOT NULL,
  ttft_ms           INTEGER
);

CREATE INDEX idx_request_log_ts     ON request_log(ts);
CREATE INDEX idx_request_log_key_ts ON request_log(key_id, ts);

-- Hourly totals outlive raw rows so charts stay long range after retention trims.
CREATE TABLE usage_hourly (
  bucket            INTEGER NOT NULL,
  key_id            INTEGER NOT NULL,
  model             TEXT    NOT NULL DEFAULT '',
  requests          INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket, key_id, model)
) WITHOUT ROWID;

-- Saved on shutdown so a restart does not hand every key a full token bucket.
CREATE TABLE limiter_state (
  key_id      INTEGER PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
  tpm_balance REAL    NOT NULL,
  saved_at    INTEGER NOT NULL
);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
