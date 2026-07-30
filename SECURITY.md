# Security

Quecto sits between the open internet and a model server that has no auth of its own. That is the whole job, so here is what it does and does not defend against.

## Reporting

Open a private security advisory on the repository. Please do not file a public issue for anything exploitable.

## Trust boundary

**Quecto speaks plain HTTP and does not terminate TLS.** Put Cloudflare, Caddy, or Traefik in front of it. Exposed directly, every API key and admin password crosses the network in cleartext. This is the single most likely way to deploy it wrong.

Set `QUECTO_TRUST_PROXY_HEADERS=true` only when something you control sits in front, since it makes Quecto believe `X-Forwarded-Proto` and `X-Forwarded-For`.

## What is protected

| | |
|---|---|
| API keys | 238 bits from `crypto/rand`, stored only as SHA-256, compared in constant time, shown once at creation |
| Admin password | argon2id, 64MB and 3 passes, concurrency-bounded so a flood cannot exhaust memory |
| Sessions | HttpOnly, `SameSite=Strict`, `Secure` when the request is HTTPS. The signing key folds in the password hash, so changing the password invalidates every existing session |
| Secrets at rest | `quecto.db` is created `0600`, and the container runs as a non-root user, so the plaintext upstream credential and signing key are not world-readable by default |
| CSRF | `SameSite=Strict` plus JSON-only request bodies. There is no synchroniser token |
| XSS | Every user-controlled string is escaped before reaching the DOM. No inline event handlers, no external scripts |
| Path traversal | The dashboard is served from an embedded filesystem, so there is no path to escape to |
| Prompt content | Never written to disk. Logs hold metadata only, and there is no setting to change that |

Fast hashing for API keys is deliberate. The key is high-entropy random, so a slow hash buys nothing against brute force and would put ~70ms on every proxied request.

## Known limitations

These are real, and accepted for now.

- **No per-IP rate limiting.** Limits are per key. An attacker with no valid key can still generate load; auth is cheap enough to absorb it, but nothing throttles by source address.
- **No login lockout.** Concurrent verifications are capped, which bounds memory and holds brute force to roughly 50 attempts per second. Use a long admin password, and prefer putting the dashboard behind your reverse proxy's own auth.
- **A compromised admin session is an SSRF pivot.** The upstream URL is editable from the dashboard, so an attacker with admin access can point Quecto at internal addresses and read the responses.
- **Secrets sit in the database in plaintext.** The upstream credential and the session signing key have to be usable. Quecto creates `quecto.db` as `0600`, but it is not encrypted, so keep it on a volume you trust.
- **Sessions are stateless, so signing out is client-side.** Logout clears the cookie in the browser, but a token that was captured stays valid until it expires, at most 14 days. Changing the admin password is what revokes every outstanding session at once.
- **No audit log** of administrative actions.
- **Single instance.** Rate limits live in memory, so running two copies gives each key its limit twice over.

## Deploying it sensibly

Terminate TLS in front. Give the container its own volume. Keep `/admin` off the public internet if you can, or behind an additional auth layer. Set expirations on keys you hand out. Watch the Logs page for `401` and `429` patterns you did not expect.
