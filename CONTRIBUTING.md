# Contributing

## Build

```bash
make build   # dashboard first, then the binary that embeds it
make test
make bench
```

The dashboard is embedded with `go:embed`, which resolves at **compile time**. After editing anything under `dashboard/` you must rebuild it before the binary, or you get a successful build serving the previous UI with no warning. `make build` does both in order.

Backend-only work needs no Node. A checkout without a built dashboard compiles and runs; the proxy serves traffic normally and `/admin` returns a page explaining what is missing.

`go run ./cmd/nullupstream` gives you a model server that answers instantly, which is what the tests and benchmarks run against.

## Scope

Quecto does three things: keys, limits, logs. It fronts one model server. These are out of scope, and a pull request adding one will be declined however good it is:

model routing or fallback, provider translation, response caching, cost and billing, teams and roles, horizontal scaling, TLS termination.

That list is roughly the feature set of a multi-provider gateway, which is the right answer when you need one. The value here is being the smallest thing that hands out keys, caps what each one spends, and records it without storing prompts, in a binary you can read in an afternoon.

## Conventions

- **Comments are stateless.** One or two lines, explaining what the code does or why. Never reference a plan, a milestone, a ticket, or anything outside the file.
- **No em dashes anywhere**, least of all in user-facing copy. Restructure the sentence instead.
- **Error messages are for the person reading them.** Say what went wrong and what to do, in plain sentences.
- Hand-written SQL, no ORM. Stdlib routing, no framework.
- **A new dependency is close to a no.** Two direct modules today, twelve in the whole build, and part of the pitch is that there is very little here to patch. A dependency has to do something the standard library genuinely cannot.
- The dashboard shares one visual vocabulary in `global.css`, so components and script-rendered rows use the same classes.

## Tests

`limit` and `proxy` carry the load-bearing behaviour and are expected to stay well covered. Anything touching the request path should also come with a benchmark, so a regression shows up as a number rather than an opinion.

**The database is never on the proxy hot path.** Keys live in a map, limiters in memory, log writes go through a buffered channel. That is what lets a single process hold authoritative limits with no Redis behind it, so a change that makes a proxied request touch disk is a change to what Quecto is.

Run `go test -race ./...` before opening a pull request.
