# Contributing to Fletcher

Thanks for your interest in Fletcher. This document covers how to get set up and
the conventions the project follows.

## License

Fletcher is licensed under the [Apache License, Version 2.0](LICENSE).

By contributing, you agree that your contributions are licensed under the same
Apache-2.0 terms (inbound = outbound). GitHub applies this to pull requests
opened against this repository by default.

## Before you start

Fletcher is opinionated about its scope and design. Two documents are worth
reading first:

- [`DESIGN.md`](DESIGN.md) for the positioning, the architecture, and the
  reasoning behind the trust boundary and the job model. Read this first if you
  are proposing anything structural.
- [`STANDARDS.md`](STANDARDS.md) for the coding conventions: repo layout, lint,
  test, error handling, logging, CLI, concurrency, dependencies, and release
  process.

If a change touches architecture, cite the relevant section of `DESIGN.md` in
your proposal so any drift is visible.

## Development

Fletcher is a single static Go binary built with `CGO_ENABLED=0`. Go 1.26+ is
required (the project uses the `tool` directive). All other build-time tools are
pinned in `go.mod` and reachable via `go tool <name>`.

```sh
make build      # local platform binary at ./bin/fletcher
make check      # lint, tests, and the generated-file drift check
make generate   # regenerate code (sqlc, buf/connect-go, mocks)
```

Run `make check` before opening a pull request.

### Documentation site

The public docs live in [`docs/site`](docs/site) and are built with VitePress,
managed by pnpm:

```sh
cd docs/site
pnpm install
pnpm dev        # local preview with hot reload
pnpm build      # production build
```

Only public, end-user material belongs in the docs site. Internal notes stay out
of it.

## Pull requests

- Keep each commit a single coherent unit of work.
- Run `make check` and make sure it passes.

## Reporting issues

Open an issue describing what you expected, what happened, and how to reproduce
it. For anything that looks security-sensitive, please do not open a public
issue. Contact the maintainer directly so it can be handled responsibly.
