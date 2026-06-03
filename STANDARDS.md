# Fletcher Standards

The "what". The "why" lives in `DESIGN.md`.

## Repo layout

- One binary `fletcher`. Subcommands distinguish role: `fletcher serve` runs the daemon; everything else is a client subcommand.
- All packages under `internal/`. Promote to `pkg/` only when there is a real external consumer.
- Component-based packages. No `domain/`, `util/`, or other catch-alls. Types live in their owning package.
- Consumers define their own narrow interfaces ("accept interfaces, return structs"). The `internal/sqlite` package exposes the sqlc-generated `*Queries`; consumers wrap it behind a local interface.

```
cmd/fletcher/
internal/
  api/         # connect-go handlers + interceptors
  job/         # job model + supervisor
  runtime/     # interface + firecracker / runc / mock drivers
  snapshot/    # interface + btrfs / mock drivers
  network/     # wireguard, UPnP/NAT-PMP, DDNS
  gateway/     # model gateway
  mcp/         # MCP server tools
  sqlite/      # connection, migrations, queries, sqlc output
  secrets/     # age
  events/      # NATS
  config/      # urfave/cli boot config wiring
  errs/        # error categories + Categorized interface
  background/  # goroutine helper (panic-safe, named)
  fname/       # caller-name helper
  proc/        # os/exec wrapper
  gen/proto/   # buf + connect-go output (single sweepable tree)
proto/         # .proto sources
embed/         # firecracker binary + rootfs assets
```

## Build & tooling

- `Makefile` at repo root. Targets: `build`, `build-linux-amd64`, `build-linux-arm64`, `test`, `lint`, `check` (lint + test), `generate`, `generate-check`, `fmt`, `cover`, `tools`, `clean`.
- `CGO_ENABLED=0` baked into build target.
- Reproducible build flags: `-trimpath`, `-ldflags="-s -w -X <pkg>/internal/buildinfo.Version=… -X …Commit=… -X …Date=…"`. Build metadata lives in `internal/buildinfo`.
- Build-time tools pinned via Go 1.24+ `tool` directives in `go.mod`. Invoked as `go tool <name>` (e.g. `go tool golangci-lint run`). No separate `tools.go` file.
- `lefthook` for git hooks. Pre-commit: `golangci-lint fmt --diff`, `go vet`. Pre-push: `make check` (lint + tests + `generate-check`). Install once per clone: `lefthook install`.
- Releases: `goreleaser`. Conventional commits feed auto-generated changelogs.
- Commit format: [Conventional Commits 1.0](https://www.conventionalcommits.org/). Enforced via `lefthook` `commit-msg` hook running `siderolabs/conform`.

## Linting & formatting

- `golangci-lint` v2.
- Formatters: `gofumpt` (stricter `gofmt` superset) + `goimports` with local prefix `github.com/joshjon/fletcher`.
- Enabled linters: `errcheck`, `govet`, `staticcheck`, `revive`, `gocritic`, `gosec`, `unconvert`, `unparam`, `misspell`, `gofumpt`, `goimports`, `nolintlint`, `errorlint`, `nilerr`, `bodyclose`, `contextcheck`, `noctx`, `prealloc`, `sqlclosecheck`, `funlen`, `gocognit`.
- Disabled: `wsl`, `nlreturn`, `gochecknoglobals`, `varnamelen` (style-noise).
- `errcheck.exclude-functions` includes `(io.Closer).Close` to silence the deferred-close pattern.
- `//nolint` directives require the linter name and a `// reason` comment (enforced by `nolintlint`).

## Testing

- Stdlib `testing` + `testify/require`. `gotestsum` for local output.
- Mocks: `mockery` v3 with `matryer` template. Config in `.mockery.yml`. Mocks live in sibling `<pkg>mock/` packages.
- Race detector always on (`-race`).
- `t.Parallel()` only for slow / IO-bound tests, not fast unit tests.
- Coverage reported via `make cover` (HTML). Not gated.
- Build tag `//go:build integration` for tests needing real `/dev/kvm`, btrfs, or network.
- SQLite tests are unit (in-memory via `modernc.org/sqlite`); no integration tag required.
- Time-dependent tests use stdlib `testing/synctest`. No `Clock` abstraction in production code - production calls `time.Now()` / `time.Sleep` / `time.After` directly.
- Property/fuzz via stdlib `testing.F` on parsers, encoders, ID code.
- End-to-end deferred until surface justifies it.

## Errors

- `fmt.Errorf("...: %w", err)` at boundaries. `pkg/errors` not used.
- Sentinels for stable conditions callers check. Typed errors only when carrying data.
- `internal/errs` defines `Category` (enum) + `Categorized` (interface) + `Wrap(err, cat)`. Domain packages depend on `internal/errs`, never on `connect-go`.
- Single Connect interceptor maps `Category → connect.Code` and sanitizes `Internal` messages before they cross the wire.
- Panics allowed only for impossible-state assertions. Recover at every goroutine root (`background.Go` does this).
- `errors.Join` for multi-error aggregation.
- Messages lowercase, no trailing punctuation, no "failed to" prefix.
- No stack traces stored on errors.

## Logging

- Custom logger interface wrapping stdlib `log/slog`, modeled on `github.com/joshjon/kit/log`.
- Key naming: `snake_case` tokens, `.`-separated groupings via `slog.Group(...)`. Never pre-baked dotted string keys.
- Standard fields: `time`, `level`, `msg`, `component`, plus `request_id` / `job_id` / `agent_id` when present.
- Sensitive values use typed wrappers whose `LogValue()` returns `"<redacted>"`.
- No always-on `caller` field - add on demand via `internal/fname`.

## Observability

- No metrics or tracing for v1.
- `net/http/pprof` on local-only `--debug` listener.
- `/healthz` (liveness) and `/readyz` (readiness checks SQLite + runtime driver) on local listener.
- Audit log deferred, but every privileged MCP tool call and approval state transition goes through a middleware seam from day one so audit becomes one method, not a refactor.

## Config

- Boot config via `urfave/cli` v3 native sources. Precedence: flag > env (`FLETCHER_*`) > TOML file > default.
- File default: `$XDG_CONFIG_HOME/fletcher/config.toml` (fallback `/etc/fletcher/config.toml`), `--config` overrides.
- Daemon collects all validation errors at startup and prints together before exit.
- Runtime-mutable settings live in SQLite `settings` table; edited via `fletcher settings get|set|list`.
- SIGHUP reloads log level only. Everything else needs restart.
- Age keyring path in config; per-value encrypted in SQLite.

## Generated code

- Generated code is committed. `make generate-check` regenerates and `git diff --exit-code`s in pre-push.
- Proto layout: `proto/fletcher/v1/*.proto` → `internal/gen/proto/fletcher/v1/`.
- sqlc layout: `internal/sqlite/queries/*.sql` → `internal/sqlite/gen/`. sqlc reads `internal/sqlite/schema.sql`, which is a generated mirror built by `make generate` by concatenating the `migrations/*.up.sql` files in order. The mirror is committed (so `go build` works on a fresh clone), but it is never hand-edited - migrations remain the source of truth.
- Mocks: `internal/<pkg>/<pkg>mock/`.
- Generated files carry `// Code generated <tool>. DO NOT EDIT.` header (golangci-lint auto-excludes).
- sqlc options: `emit_interface: true`, `emit_pointers_for_null_types: true`, `emit_json_tags: false`.
- No scattered `//go:generate` directives - single `make generate` drives every tool from its own config.
- `buf breaking` deferred until first release.

## Migrations & SQL

- `golang-migrate`. Files in `internal/sqlite/migrations/`, named `0001_<desc>.up.sql` / `0001_<desc>.down.sql`.
- Sequential 4-digit numeric prefixes. Never timestamps.
- Embedded in binary via `embed.FS`; daemon runs `migrate.Up()` on startup.
- Down migrations required for every up; production never auto-rolls back. Fix forward with a new up.
- Data transformations beyond what SQL can express cleanly run as Go functions tracked in a `migration_data` table. SQL stays declarative.
- All tables `STRICT`.
- Time columns: `INTEGER` Unix epoch (seconds, or ms when sub-second matters).
- Boolean columns: `INTEGER` with `CHECK (col IN (0, 1))`. Naming convention `is_*` / `has_*` → sqlc override maps to Go `bool`.
- `PRAGMA foreign_keys = ON` and `journal_mode = WAL` at connection open.
- One `.sql` file per entity. Explicit columns only; no `SELECT *`.
- sqlc query naming: `-- name: <Verb><Entity> :<one|many|exec>`. Verbs: `Get`, `List`, `Create`, `Update`, `Delete`, `Count`.
- Real deletes by default. `deleted_at` only on tables that demonstrably need history.

## CLI

- Library: `urfave/cli` v3.
- Subcommand shape: two-word commands, singular resource + verb (`fletcher job list`, not `fletcher jobs`).
- Verbs: `list`, `get`, `create`, `update`, `delete`, `cancel`. Full words, consistent everywhere.
- Output: human table by default. Global `-o json|yaml|jsonl` flag. `-q` for ID-only.
- Exit codes: 0 ok, 1 usage error, 2 daemon unreachable, 3 not found, 4 conflict, 5 permission denied.
- Boolean flags: `--foo` / `--no-foo`.
- List flags are repeatable (`--label k=v --label k2=v2`), not comma-separated.
- Color auto on TTY; `NO_COLOR` respected.
- Destructive ops prompt for confirmation; `-y` / `--yes` bypasses.
- CLI ⇄ daemon over Unix socket by default. TCP only for remote (over WireGuard).
- No man pages for v1 - `--help` suffices.

## Concurrency

- `oklog/run.Group` owns top-level service lifecycle. SIGINT / SIGTERM → 30s graceful shutdown, then force-exit.
- `internal/background.Go(ctx, fn)` for all goroutines. Auto-derives name from caller via `internal/fname`; recovers panics with structured log. Use `GoNamed(ctx, name, fn)` only when caller-derived name is ambiguous (e.g. a loop spawning N goroutines).
- `golang.org/x/sync/errgroup` for scoped fan-out where parent waits and errors propagate.
- `pond` added only when a real bounded-worker-pool need appears - not pre-emptively.
- Context-first arg convention. `context.Value` reserved for request-scoped data (request_id, job_id, agent_id). Dependencies are constructor-injected struct fields.
- No bare `go func()` in production code.
- Backoff: `cenkalti/backoff` with exponential + jitter. Defaults: base 200ms, max 30s, max-elapsed 5min. Idempotency keys on every retry per `DESIGN.md` §5.
- Rate limiting: `golang.org/x/time/rate` token bucket where needed.

## Documentation

- `README.md`: what / install / quickstart / link to `DESIGN.md`. Minimal.
- No `CONTRIBUTING.md` / `SECURITY.md` for v1. No separate ADR system - `DESIGN.md` + conventional commits play that role.
- `doc.go` per package: `// Package foo does X.` one paragraph.
- One-line godoc on every exported identifier. Subject + verb, period.
- No multi-paragraph godocs. If something needs that much explanation, the design is unclear.
- `// TODO: <description>` plain. Single marker - no `// FIXME` / `// XXX`.
- `CHANGELOG.md` auto-generated by goreleaser at release time from conventional commits.

## Dependencies

- Permissive licenses only: MIT, BSD-2/3, Apache 2.0, ISC, MPL 2.0. No GPL / AGPL / LGPL family.
- No deps that phone home or require an external runtime binary on the user's box.
- Default answer is "no": stdlib first, in-repo helper second, dep last.
- Pin exact versions (Go module default). `go.sum` committed.
- Avoid `replace` directives; if unavoidable, comment in `go.mod` explaining why and when removable.
- No vendor directory.

## Versioning & release

- Semver `vMAJOR.MINOR.PATCH`. Start at `v0.1.0`; `v1.0.0` when the design stabilizes and we commit to wire compatibility.
- Linux amd64 + arm64 only. No macOS / Windows artifacts.
- Pre-v1: `SHA256SUMS` for integrity. Cosign keyless signing added at `v1.0.0`. No GPG.
- Distribution: GitHub Releases.
- `curl | sh` install script: detects arch, fetches latest, verifies SHA, installs to `/usr/local/bin/fletcher`, drops systemd unit at `/etc/systemd/system/fletcher.service`.
- Tag-driven releases from `main` only.
- Build info via `-ldflags`: `version`, `commit`, `date`. Exposed via `fletcher version [--json]`.
- `.goreleaser.yaml` at repo root. `goreleaser release --clean --snapshot` for local dry-runs.

## Utilities

- IDs: `jetify-com/typeid-go`.
- Validation: `bufbuild/protovalidate-go` via Connect interceptor; rules in `.proto`.
- HTTP client: stdlib `net/http` + `cenkalti/backoff`. Default 60s timeout for LLM calls.
- Atomic file writes: `google/renameio/v2`.
- Subprocess: `internal/proc/` thin wrapper around stdlib `os/exec` - context-aware cancellation, structured-log streaming of stdout/stderr with correlation IDs, process-group setup so killing parent kills children.
- JSON: stdlib `encoding/json` (move to `v2` when stable).
- FS: stdlib `io/fs.FS` for reads; `*os.Root` for write paths.
- Clock: none - stdlib `testing/synctest` for time-dependent tests; production code calls `time` package directly.

## Mac dev

- The pure-Go bulk of the daemon (`CGO_ENABLED=0`, `modernc.org/sqlite`, `slog`, Connect, NATS) compiles and runs on macOS unchanged.
- `runtime.MockDriver` and `snapshot.MockDriver` are required production-code citizens behind the `DESIGN.md` §10 seams - not test hacks. They let the daemon's coordination logic run end-to-end on macOS.
- Real Firecracker / btrfs / WireGuard work: cross-compile (`make build-linux-arm64`) and run inside an arm64 Linux VM (UTM on M1).
