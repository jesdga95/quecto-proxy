export interface Key {
  id: number;
  name: string;
  key_prefix: string;
  created_at: number;
  expires_at: number | null;
  revoked_at: number | null;
  rpm_limit: number | null;
  tpm_limit: number | null;
  total_token_limit: number | null;
  last_used_at: number | null;
  status: "active" | "expired" | "revoked" | "exhausted";
  total_requests: number;
  total_prompt_tokens: number;
  total_completion_tokens: number;
  live: {
    rpm_used: number;
    rpm_remaining: number;
    rpm_exhausted: boolean;
    tpm_used: number;
    tpm_remaining: number;
    tpm_exhausted: boolean;
    total_spent: number;
    total_remaining: number;
    total_exhausted: boolean;
  };
}

/** The fields a key edit may carry. Every one is optional: a field left out is
 * left alone, and a limit is taken away with its clear flag rather than by
 * sending a number, so an empty box and no box at all stay distinguishable. */
export interface KeyPatch {
  name?: string;
  expires_at?: number;
  rpm_limit?: number;
  tpm_limit?: number;
  total_token_limit?: number;
  clear_expiry?: boolean;
  clear_rpm_limit?: boolean;
  clear_tpm_limit?: boolean;
  clear_total_token_limit?: boolean;
  reset_tokens_spent?: boolean;
}

export interface LogEntry {
  id: number;
  key_id: number;
  key_name: string;
  ts: number;
  model: string;
  status: number;
  stream: boolean;
  prompt_tokens: number | null;
  completion_tokens: number | null;
  cached_tokens: number | null;
  duration_ms: number;
  ttft_ms: number | null;
}

export interface Bucket {
  bucket: number;
  requests: number;
  errors: number;
  tokens: number;
}

export interface Stats {
  from: number;
  to: number;
  series_step: number;
  totals: {
    requests: number;
    errors: number;
    prompt_tokens: number;
    completion_tokens: number;
    cached_tokens: number;
    cache_known_prompt_tokens: number;
  };
  series: Bucket[] | null;
  top_keys:
    | {
        key_id: number;
        key_name: string;
        requests: number;
        tokens: number;
      }[]
    | null;
  ttft_p95_ms: number | null;
  duration_p95_ms: number | null;
  proxy_overhead_p99_ms: number;
}

export interface Health {
  upstream: string;
  upstream_ok: boolean;
  upstream_error?: string;
  checked_at: number;
  usage_reporting_known: boolean;
  usage_reporting_supported: boolean;
  logs_dropped: number;
  log_rows: number;
  database_bytes: number;
  key_count: number;
  proxy_overhead_p99_ms: number;
}

export interface Settings {
  upstream_url: string;
  upstream_api_key: string;
  default_max_tokens: number;
  log_retention_days: number;
  log_max_rows: number;
  listen: string;
  admin_listen: string;
  db_path: string;
  upstream_header_timeout: string;
  trust_proxy_headers: boolean;
}

const BASE = "/quecto/api";

/** Thrown for any non-2xx reply, carrying the message the server wrote. */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    credentials: "same-origin",
    // The API refuses any write not sent as JSON, even one with no body.
    headers: { "Content-Type": "application/json" },
    ...init,
  });

  if (res.status === 401 && !path.startsWith("/session")) {
    window.location.href = "/admin/login";
    throw new ApiError(401, "Sign in to continue.");
  }
  if (res.status === 204) return undefined as T;

  const text = await res.text();
  let body: any = {};
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      // Something other than the API answered. Carry its words rather than
      // failing on the parse and losing the status with them.
      body = { error: text.trim() };
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, body.error || "Something went wrong.");
  }
  return body as T;
}

export const api = {
  login: (password: string) =>
    request<{ ok: boolean }>("/session", {
      method: "POST",
      body: JSON.stringify({ password }),
    }),

  logout: () => request<{ ok: boolean }>("/session", { method: "DELETE" }),

  me: () => request<{ authenticated: boolean; key_count: number }>("/me"),

  keys: () => request<{ keys: Key[] }>("/keys"),

  createKey: (body: {
    name: string;
    expires_at: number | null;
    rpm_limit: number | null;
    tpm_limit: number | null;
    total_token_limit: number | null;
  }) =>
    request<{ key: Key; secret: string }>("/keys", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  revokeKey: (id: number) =>
    request<Key>(`/keys/${id}/revoke`, { method: "POST" }),

  rotateKey: (id: number) =>
    request<{ key: Key; secret: string }>(`/keys/${id}/rotate`, {
      method: "POST",
    }),

  updateKey: (id: number, body: KeyPatch) =>
    request<Key>(`/keys/${id}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),

  deleteKey: (id: number) => request<void>(`/keys/${id}`, { method: "DELETE" }),

  logs: (params: Record<string, string>) => {
    const q = new URLSearchParams(
      Object.entries(params).filter(([, v]) => v !== ""),
    );
    return request<{ logs: LogEntry[] | null; next_cursor: number | null }>(
      `/logs?${q}`,
    );
  },

  stats: (range: string) => request<Stats>(`/stats?range=${range}`),

  health: () => request<Health>("/health"),

  settings: () => request<Settings>("/settings"),

  updateSettings: (body: Record<string, unknown>) =>
    request<Settings>("/settings", {
      method: "PATCH",
      body: JSON.stringify(body),
    }),
};
