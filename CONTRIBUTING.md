# Contributing

The main contribution guide lives in the
[`silo` repository](https://github.com/silopost/silo/blob/main/CONTRIBUTING.md);
it is the source of truth. Short version:

- Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, ...).
- Run `make lint test` before opening a PR.
- Keep PRs focused; CI must be green.

## API compatibility and releases

- Changes to the REST API (`api/openapi.yaml`), error codes and SSE event types
  are additive by default: new endpoints, optional fields, new codes or event
  types that older clients can ignore.
- A release that breaks the contract for older web clients (removes or
  changes an endpoint, field, error code or event they use) raises
  `MinWebVersion` in `internal/http/api/api.go` to the first silo-web release
  that handles the change, and says so in the release notes. `GET /meta`
  reports it as `minWebVersion`; older web clients then ask the user to
  reload.
- Database migrations stay backward compatible within a release (plan 2.4);
  `scripts/upgrade-test.sh` (CI: `.github/workflows/upgrade.yml`) upgrades a
  seeded instance from the previous tag to the current tree.
