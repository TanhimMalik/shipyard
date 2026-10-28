# Shipyard

Shipyard is a single-host deployment platform built with Go, Next.js, PostgreSQL,
and Docker. The frontend will run on Vercel; the deployment engine will run on one
Linux host.

It's still in development. Project screens and the local API work. The optional
worker can fetch approved repositories, build their Dockerfiles, start containers,
check HTTP readiness, and save the observed state. Passed checks stay in
`HEALTH_CHECKING` until routing is implemented. UI deployment controls and remote
admin access also come next.

The Traefik adapter and persisted activation contract are tested separately;
see [local routing](docs/routing.md). Connecting activation to the worker is next.

## Run locally

You'll need Node 24, pnpm 12.9.1, Go 1.27.2 or newer in the Go 1.27 series, and
Docker with Compose. Source checkout also needs Git 2.39+ with libcurl 7.59+ on
macOS or Linux.

From the repository root, start PostgreSQL, apply migrations, and run the API:

```sh
test -f infrastructure/.env || (
  umask 077
  printf 'SHIPYARD_DB_PASSWORD=%s\n' "$(openssl rand -hex 32)" > infrastructure/.env
)
docker compose --env-file infrastructure/.env -f infrastructure/compose.yaml up -d --wait
export SHIPYARD_DATABASE_URL="postgres://shipyard:$(sed -n 's/^SHIPYARD_DB_PASSWORD=//p' infrastructure/.env)@127.0.0.1:5432/shipyard?sslmode=disable"
cd apps/api
go run ./cmd/migrate up
go run ./cmd/server
```

In another terminal, from the repository root:

```sh
pnpm install
pnpm dev
```

Open http://127.0.0.1:3000. The API listens on `127.0.0.1:8080`; `/api/health`
reports process liveness. Project and deployment endpoints currently accept local
requests only. Editing, deletion, and deployment queueing are API-only.

The database password file is ignored by Git. The API reads its database URL from
the environment, not from `.env` files. PostgreSQL data persists in a Docker volume.
Stop it with `docker compose --env-file infrastructure/.env -f infrastructure/compose.yaml down`
from the repository root.

## Deployment worker

The worker is off by default. Set these before starting the API to enable it:

- `SHIPYARD_EXECUTOR_ENABLED=true`
- `SHIPYARD_WORKSPACE_ROOT`: an absolute directory owned by the service user with
  permissions `0700`.
- `SHIPYARD_ALLOWED_REPOSITORIES`: a JSON array of exact approved HTTPS Git URLs.
- `SHIPYARD_DOCKER_HOST`: an explicit local Unix socket URL, such as
  `unix:///var/run/docker.sock`. The CLI context and ambient `DOCKER_HOST` are ignored.

Use Linux Docker Engine 28+ with BuildKit. One worker processes one attempt at a
time, using a dedicated PostgreSQL session lock. It needs a direct database
connection with at least two pool connections.

Containers publish only the app port on the Docker host's loopback address and
run with CPU, memory, process, and capability limits. Docker access is privileged;
this is for approved, controlled repositories, not hostile code.

Readiness probes use the Docker host's loopback port and configured path. They
accept HTTP 2xx, retry for up to 30 seconds by default, and reject redirects.
The worker verifies the same container and port before and after probing, then
saves the final result. A passed check does not activate a route.

Startup failures clean up verified owned resources before recording failure.
Uncertain Docker or database results preserve the unfinished attempt for recovery.
On restart, the worker cleans interrupted source, build, startup, and readiness
attempts; it does not resume them. Later lifecycle stages are not handled yet. Abrupt crashes
can leave source directories or late Docker resources requiring manual cleanup
while the worker is stopped. Build cache is retained.

## Vercel

Import the repository with root directory `apps/web`, framework **Next.js**, and
Node.js **24.x**. Enable **Include source files outside of the Root Directory in
the Build Step**, keep system environment variables enabled, and set
`ENABLE_EXPERIMENTAL_COREPACK=1`. The included `vercel.json` supplies build commands.

Leave `SHIPYARD_API_URL` unset until the Linux API has a reachable HTTPS origin.
Then set it as a server-only variable and redeploy. Vercel cannot reach your local
API. Project routes remain disabled on Vercel until protected admin access is
implemented. Cloud deployment has not been verified yet.

## Checks

From the root: `pnpm check`, `pnpm test`, and `pnpm build`.

From `apps/api`: `go vet ./...`, `go test -race ./...`, and `go build ./...`.
Run `gofmt -l .` to check Go formatting.

Integration tests use `go test -race -tags=integration ./...` with
`SHIPYARD_TEST_DATABASE_URL` pointing to local PostgreSQL. They create isolated
schemas. Set `SHIPYARD_TEST_DOCKER_HOST` to a disposable Linux engine to include
build, runtime, and readiness checks. HTTP checks need access to that host's loopback;
`SHIPYARD_TEST_DOCKER_HTTP_PROXY` accepts a SOCKS5 URL for VM setups. Set
`SHIPYARD_TEST_EXECUTOR_GIT_URL` to an approved public repository to include real
worker checkout.
