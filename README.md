# PostgreSQL Manager

Small self-hosted dashboard for PostgreSQL login users, databases, and shared full-access ownership. It queries PostgreSQL directly and does not need an application database.

## Features

- Live dashboard, login-user list, database list, and database-to-user mappings.
- Create restricted PostgreSQL login users.
- Create databases owned by a `<database>__owner` `NOLOGIN` role.
- Preview legacy database ownership migration.
- Atomically migrate supported application objects and grant shared full access.
- Preserve existing ACL grants and previous object owners.
- Skip system schemas and extension-managed objects.
- No dashboard login, cookies, audit storage, or analytics.

Put this service behind a reverse proxy restricted to trusted local IPs. The server still uses an in-memory CSRF token and restrictive browser security headers.

## Required PostgreSQL access

Use an administrative PostgreSQL role capable of creating roles/databases, changing ownership, granting role membership, and changing per-role database settings. A PostgreSQL superuser is the simplest option for a trusted internal deployment.

Configuration uses normal libpq-style environment variables:

```env
PGHOST=127.0.0.1
PGPORT=5432
PGUSER=postgres
PGPASSWORD=secret
PGDATABASE=postgres
PGSSLMODE=prefer
LISTEN_ADDR=:8080
```

`DATABASE_URL` is also supported. When set, it becomes the base connection configuration.

## Run locally

```bash
go run -buildvcs=false ./cmd/pgadmin-manager
```

Open <http://127.0.0.1:8080>.

## Docker

Published images use this path:

```text
ghcr.io/<github-owner>/<repository>:latest
```

Set the exact image in `.env`, then start Compose:

```bash
cp .env.example .env
docker compose -f compose.example.yaml pull
docker compose -f compose.example.yaml up -d
```

Example `.env` value:

```env
PGADMIN_MANAGER_IMAGE=ghcr.io/your-github-username/pgadmin-manager:latest
```

The example binds the dashboard only to `127.0.0.1:8080`. On Linux, `host.docker.internal` is mapped to the Docker host. PostgreSQL must accept the container connection according to `listen_addresses` and `pg_hba.conf`.

If PostgreSQL is another Compose service, put both services on the same network and set `PGHOST` to that service name instead.

## GitHub Container Registry

The workflow in `.github/workflows/container.yml` validates the Docker build for pull requests targeting `main`. A merge into `main` creates and pushes multi-platform images for `linux/amd64` and `linux/arm64`.

Published tags:

- `latest`
- `main`
- `sha-<short-commit>`

The workflow authenticates with the repository's automatic `GITHUB_TOKEN`; no personal access token is required. It needs repository workflow permission to write packages. After the first publish, set the GHCR package visibility to **Public** if anonymous Docker pulls are required.

## Migration behavior

Before granting access, open **Databases → Manage access**. The dashboard shows current owner, target shared role, object counts, previous owners, and extension objects skipped.

After confirmation, one PostgreSQL transaction:

1. creates or validates the shared `NOLOGIN` owner role;
2. preserves old owners through role membership;
3. moves supported non-system objects to the shared owner;
4. adds the requested user to the shared role;
5. configures automatic `SET ROLE` for that database;
6. changes database ownership.

Existing ACLs are not revoked. Supported objects include schemas, tables, partitions, views, materialized views, sequences, foreign tables, functions, procedures, aggregates, domains, and user-defined types. Extension-managed objects are skipped.

Reconnect the target PostgreSQL user after granting access so its database-specific role setting takes effect.

## Checks

```bash
go test ./...
go build -buildvcs=false ./cmd/pgadmin-manager
docker build -t pgadmin-manager:local .
```
