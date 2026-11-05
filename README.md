# silo-server

Backend of SiloPost, a self-hosted web mail client (IMAP/SMTP). IMAP stays the
source of truth; Silo is a client. License: AGPL-3.0.

Architecture and roadmap: see `ARCHITECTURE_AND_PLAN.md` in the
[`silo`](https://github.com/silopost/silo) repository.

## Requirements

- Go 1.26+
- Docker (optional, for image builds)
- golangci-lint v2 (optional, for `make lint`)

## Run

```sh
make run
curl localhost:8080/healthz   # {"status":"ok"}
```

Other targets: `make build`, `make test`, `make lint`.

Docker: `docker build --target prod -t silo-server .` (or `--target dev` for
hot reload with air; mount the repo at `/src`).

## Configuration (Phase 0)

| Variable | Default | Meaning |
|---|---|---|
| `SILO_HTTP_ADDR` | `:8080` | HTTP listen address |
| `SILO_LOG_FORMAT` | `json` | `json` or `text` |
| `SILO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

More variables are added as features land.
