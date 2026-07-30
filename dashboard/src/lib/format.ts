export function count(n: number): string {
  return n.toLocaleString("en-US");
}

/** Compact token counts, since millions are common and columns are narrow. */
export function tokens(n: number): { value: string; unit: string } {
  if (n >= 1_000_000) return { value: (n / 1_000_000).toFixed(2), unit: "M" };
  if (n >= 1_000) return { value: (n / 1_000).toFixed(1), unit: "k" };
  return { value: String(n), unit: "" };
}

export function tokensFlat(n: number): string {
  const { value, unit } = tokens(n);
  return value + unit;
}

export function millis(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return "";
  if (ms >= 1000) return (ms / 1000).toFixed(2) + "s";
  return Math.round(ms) + "ms";
}

export function clockTime(unix: number): string {
  return new Date(unix * 1000).toLocaleTimeString("en-GB", {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function date(unix: number | null): string {
  if (!unix) return "";
  return new Date(unix * 1000).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
  });
}

export function ago(unix: number | null): string {
  if (!unix) return "Never";
  const secs = Math.floor(Date.now() / 1000) - unix;
  if (secs < 60) return "Just now";
  if (secs < 3600) return `${Math.floor(secs / 60)} min ago`;
  if (secs < 86400) {
    const h = Math.floor(secs / 3600);
    return h === 1 ? "1 hour ago" : `${h} hours ago`;
  }
  const d = Math.floor(secs / 86400);
  if (d === 1) return "Yesterday";
  if (d < 60) return `${d} days ago`;
  return `${Math.floor(d / 30)} months ago`;
}

/** Days until expiry, negative once it has passed. */
export function daysUntil(unix: number): number {
  return Math.ceil((unix - Date.now() / 1000) / 86400);
}

export function bytes(n: number): string {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GB";
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(0) + " KB";
  return n + " B";
}

export function statusClass(status: number): string {
  if (status >= 500) return "code-5xx";
  if (status >= 400) return "code-4xx";
  return "code-2xx";
}

/** Escapes text before it goes into innerHTML. */
export function esc(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}
