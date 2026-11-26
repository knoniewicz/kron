# Kron

Collector host: one long-lived Kor service named `kron` that runs Lua connectors and
automations. Two trigger kinds only: time (`at`/`every`) and event (`on`). No UI.

    kron -kor 127.0.0.1:4730 -dir ./lua/examples      # env: KOR_ADDR, KRON_DIR, KRON_ENV_ALLOW (names Lua may read)
    kill -HUP <pid>                          # reload lua files; a broken file keeps the old jobs

Files: `<dir>/connectors/*.lua` and `<dir>/automations/*.lua`. Job names are unique.

## Defining jobs

    connector "name"  { every = "30m" | at = "06:00", run = function(ctx) ... end }
    automation "name" { every = "1h" | at = "06:00" | on = "subject.or.prefix.*",
                        catchup = true, run = function(ctx, event) ... end }

Exactly one of `every` (Go duration), `at` (local HH:MM, daily) or `on` (Kor subject; a trailing
`.*` matches the prefix). Connectors are time-only. `event` is `{subject=, payload=}`.
`catchup = true` (daily `at` jobs): on startup, run once if the last successful `automation.run`
is older than the most recent scheduled time. A timer run that fires while the job is still running is
skipped; event runs and `kron.run` wait for it.

## ctx

| call | does |
|---|---|
| `ctx.log(...)` | log line |
| `ctx.http_get(url, {headers=, timeout=})` / `ctx.http_post(url, body, opts)` | `{status, body, headers}`; table body is sent as JSON; raises on transport errors |
| `ctx.json_decode(s)` / `ctx.json_encode(v)` | JSON |
| `ctx.sha1(s)` / `ctx.sha256(s)` | hex digest (use for deterministic ids) |
| `ctx.now()` / `ctx.today()` | RFC3339 / `YYYY-MM-DD`, local time |
| `ctx.env(name)` | only names in `KRON_ENV_ALLOW` / `-env-allow`, else raises. Never log values |
| `ctx.call("svc.handler", payload)` | result table; raises `"<code>: <message>"` (Kor typed error). Use `pcall` |
| `ctx.publish(subject, payload)` | publishes `kron.<subject>` |
| `ctx.write(type, id, data, {source=})` | `korpus.write`, acked after commit; retried with backoff on transport errors or while korpus is absent; typed errors (invalid data) raise at once. Same id = upsert |
| `ctx.query(type, {where=, order_by=, desc=, limit=, offset=, since=})` | `korpus.query` -> `{records, count}` |
| `ctx.ask(prompt, data, {timeout=})` | `agent.ask` (any service that registers it); returns `text, true`, or `data, false` if the agent is unavailable. Caller decides |

## Sandbox and limits

Only base (without `dofile`, `loadfile`, `load`, `loadstring`, `print`), `string`, `table`, `math`.
No `io`, `os`, `package`/`require`, `debug`. Network only through `ctx.http_*`. Each run has a
timeout (default 2m, `-timeout`) enforced in the VM and on every call; panics are recovered.

## Run records and handlers

Every run writes Korpus `automation.run {name, kind, started, duration_ms, ok, error?}`
(id `name@started`); if Korpus is down it is logged and Kron carries on.

- `kron.list {}` -> `{items: [{name, kind, every, at, on, catchup, file}]}`
- `kron.run {name}` -> `{run}` (runs now, synchronously)
- `kron.runs {name?, limit?}` -> `{runs}` (newest `automation.run` records, via `korpus.query`)
- `kron.source {name}` -> `{name, file, lua}` (read-only: the loaded job's own Lua file as Kron loaded it, max 256 KiB; unknown names are `not_found`)

Build/test: `CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...`.

## Add your own connectors

Kron ships the engine only. Your jobs stay outside this repo:

    mkdir -p ~/.config/kron/lua/connectors ~/.config/kron/lua/automations
    cp lua/examples/connectors/hello.lua ~/.config/kron/lua/connectors/
    kron -dir ~/.config/kron/lua          # or KRON_DIR

`lua/connectors/` and `lua/automations/` in a checkout are gitignored, so the default `-dir ./lua` works too.

- Secrets: put them in the environment and list the names in `KRON_ENV_ALLOW`; read them with `ctx.env`.
- Record types: `ctx.write` needs a schema that Korpus knows. `schemas/*.json` is embedded and registered at
  start; for your own type call `ctx.call("korpus.register", {type = "my.type", schema = {...}})` or
  `PUT /v1/types/my.type` on the Korpus gateway once.
- Reload with `kill -HUP` after editing. A broken file keeps the old jobs running.

## The scheduler

Cron entries and Kor subscriptions start runs. A timer run is skipped while the job is still running; event
runs wait their turn. Every run
leaves an `automation.run` record. `catchup` runs a missed daily job once at startup. See `docs/ARCHITECTURE.md`.

## License

MIT, see [LICENSE](LICENSE).
