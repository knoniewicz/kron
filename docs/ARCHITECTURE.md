# Kron architecture

How Kron hangs together, for someone who wrote none of it. Kron is one Go binary (`main.go`)
that runs as a long-lived Kor client named `kron`. There is no UI and no other process.
Everything it does is a Kor handler call, a pub/sub subscription, or a Korpus read/write, and
all three go through the Kor daemon (`KOR_ADDR`, default `127.0.0.1:4730`).

## What runs

`main.go` dials Kor, builds `NewKron(...)` and `NewScheduler(...)`, then:

1. `registerSchemas` (schemas.go) pushes `schemas/*.json` into Korpus (`korpus.register`),
   retrying with backoff while Korpus is unreachable.
2. `registerHandlers` (main.go) registers the RPC surface: `kron.list`, `kron.run`,
   `kron.runs`, `kron.source`, `kron.decision.evaluate`. Kor prefixes handler subjects with
   the client name, so callers reach them as `kron.<name>`.
3. `s.Reload()` (sched.go) compiles `lua/connectors/*.lua` and `lua/automations/*.lua` into
   `Job` structs (job.go, `loadFile`), adds a cron entry per time trigger and a Kor
   subscription per `on` pattern. The first load also starts catch-up runs.
4. `SIGHUP` re-runs `Reload`; a broken Lua file fails the load and keeps the old jobs.

## A run

`Kron.Run` (kron.go) calls `Job.execute` (job.go), which takes the job's mutex, sets the
timeout (`-timeout`, default 2 min) on the Lua VM, recovers panics, and calls the job's
`run(ctx[, event])`. Afterwards it writes an `automation.run` record
`{name, kind, started, duration_ms, ok, error?}` with id `name@started`. If Korpus is down the
record is logged and dropped.

Each job keeps one Lua VM for its lifetime, so Lua globals survive between runs of the same job.

Overlap: a timer run (`runScheduled`) is skipped when the job is still running. Event runs
(`dispatch`) and `kron.run` wait for the job's mutex instead.

## The sandbox (sandbox.go)

Only the base, `string`, `table` and `math` libraries, without `dofile`, `loadfile`, `load`,
`loadstring` and `print`. No `io`, `os`, `require` or `debug`. Every side effect goes through
the `ctx` table: `http_get`/`http_post`, `call` (any Kor handler), `publish`, `write`/`query`
(Korpus), `ask` (`agent.ask`, if a service registers it), and `env` (only names in
`KRON_ENV_ALLOW`).

`ctx.write` goes through `Kron.write`: four attempts with exponential backoff on transport
errors or while Korpus is absent; a typed error (bad data) fails at once.

## The optional decider (decide.go)

`kron.decision.evaluate` validates bounded questions (noul | choice | score) and POSTs them to
`KRON_DECIDER_URL/decide`. Nothing else in Kron calls it. When the URL is unset or unreachable
the caller gets a typed error, never a negative verdict.

## Where to change what

| You want to change… | Touch |
|---|---|
| Triggers, cron handling, catch-up | `sched.go` (`Reload`, `runScheduled`, `dispatch`, `catchup`), `job.go` (`parseTrigger`) |
| Handler surface and payloads | `main.go` (`registerHandlers`), `handlers.go`, `source.go` |
| Korpus write retry and record plumbing | `kron.go` (`write`, `call`), `record.go` |
| Lua API and sandbox limits | `sandbox.go` (`newState`, `runCtx.table`), `job.go` (`Job.execute`) |
| Decider behaviour | `decide.go` |
| Actual jobs | your own `-dir` (`connectors/`, `automations/`); examples in `lua/examples` |
| Deployment env | `deploy/systemd/kron.service` |
