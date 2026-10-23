# Shipyard

Shipyard is a single-host deployment platform in development. So far it has a Go
API and a Next.js UI with dark and light themes. The Overview checks whether
the Go API is responding. Local project screens let you create a project, browse
the directory, and view its saved settings. The API also supports editing and
deleting configuration. Deployment attempts can be queued and inspected through
the local API. An optional worker fetches approved repositories and builds their
Dockerfiles in the background. It saves the image and a planned runtime instance;
connecting container startup comes next.

The planned hosting setup is a Next.js frontend on Vercel and a Go deployment
engine on one Linux Docker host. Vercel configuration is included; everything
currently runs locally.

## Run locally

Use Node 24, pnpm 12.9.1, and Go 1.27.2 or newer in the Go 1.27 release series.
From the repository root:

```sh
pnpm install
pnpm dev
```

Open http://127.0.0.1:3000. Set up the database below, then run the API in a
terminal with `SHIPYARD_DATABASE_URL` exported:

```sh
cd apps/api
go run ./cmd/server
```

`GET http://127.0.0.1:8080/api/health` returns `200 OK` and `{"status":"ok"}`.
This checks process liveness only. `HEAD` is also supported; other methods return
`405` and unknown paths return `404`.

Set `SHIPYARD_API_ADDR` to change the API address, for example `127.0.0.1:9090` or
`[::1]:8080`. Only literal loopback addresses and ports from 1 to 65535 are accepted.
Ctrl+C or SIGTERM allows active requests up to five seconds to finish before the
API closes remaining connections. Startup and shutdown failures exit nonzero.

The browser checks the Next.js `/api/health` route, which calls the Go endpoint.
It shows checking, available, or unavailable, with a button to check again. This
reports API liveness only and does not monitor deployed applications.

For a different API address, set `SHIPYARD_API_URL` in `apps/web/.env.local` using
`.env.example` as a reference, then restart Next.js. Use an HTTP(S) origin such as
`http://127.0.0.1:9090`, without a path, credentials, query, or fragment. Development
defaults to `http://127.0.0.1:8080`; production requires an explicit value. This
server-only setting is not sent to the browser. Requests time out after three
seconds, and failures return an uncached `503` without exposing upstream details.

Geist and JetBrains Mono load through `next/font`; builds need access to Google
Fonts. The theme defaults to dark and remembers changes in the browser.

## Database

Docker with Compose is required. From the repository root, create the ignored
`infrastructure/.env`, start PostgreSQL on `127.0.0.1:5432`, and apply migrations:

```sh
test -f infrastructure/.env || (
  umask 077
  printf 'SHIPYARD_DB_PASSWORD=%s\n' "$(openssl rand -hex 32)" > infrastructure/.env
)
docker compose --env-file infrastructure/.env -f infrastructure/compose.yaml up -d --wait
export SHIPYARD_DATABASE_URL="postgres://shipyard:$(sed -n 's/^SHIPYARD_DB_PASSWORD=//p' infrastructure/.env)@127.0.0.1:5432/shipyard?sslmode=disable"
cd apps/api
go run ./cmd/migrate up
go run ./cmd/migrate status
```

The API and migration command read `SHIPYARD_DATABASE_URL` from the environment;
they do not load `.env` files. API startup verifies the database connection within
five seconds and exits nonzero if it cannot connect. Migrations run separately.
The migration command uses pgx and Goose with embedded SQL, transactions, and a
PostgreSQL advisory lock. Repeating `up` preserves data and skips applied
migrations. The command has a 30-second deadline and supports only `up` and
`status`.

The initial schema contains only projects, with a unique lowercase slug, source
configuration, app port, readiness path, and timestamps. Schema checks cover basic
bounds; the API also validates repository URLs, branch names, and health paths.
Update queries set `updated_at` explicitly. While the API is running,
its health endpoint reports process liveness even if the database goes down.

Data persists in a named volume. From the repository root, stop PostgreSQL with
`docker compose --env-file infrastructure/.env -f infrastructure/compose.yaml down`.
Adding `-v` deletes its data. Changing the environment password does not change
credentials in an initialized database. This Compose setup is for local
development; its database role has administrative privileges.

## Project API

`POST /api/projects` accepts one JSON object with `name`, `slug`, `git_url`,
`branch`, `app_port`, and `health_path`. Text is trimmed; slugs use lowercase
letters and numbers with single hyphens. Repository URLs must use HTTPS without
credentials, queries, or fragments. Branches must be bare branch names; health
paths must start with `/` without queries, fragments, or traversal. All fields
are required. This stores configuration; it does not fetch or build the source.
Repository hosts use ASCII DNS labels (use punycode for international domains)
or canonical IP addresses. Trailing host dots and legacy numeric IPv4 spellings
are rejected. A saved URL does not imply source approval: checkout requires an
exact match in the operator's approved list, including URL casing, port, and path.
URLs are trimmed when saved; they are not otherwise rewritten. Approval is not
checked when queueing yet because the executor configuration is not connected.

Creation returns the saved project with its ID and timestamps (`201`). Invalid
settings return field errors (`422`), and a duplicate slug returns `409`.
Create and update requests require `application/json` and are limited to 64 KiB.
Unknown fields, null values, malformed JSON, and trailing objects return `400`.

`GET /api/projects` returns `projects` and `next_offset` (null on the last page).
It lists newest first, with IDs breaking timestamp ties. `limit` defaults to 50
and accepts 1–100; `offset` defaults to zero. Offset pagination may shift when
new projects are created between requests. Database failures return an uncached
`503`; each database operation has a three-second deadline.

`GET /api/projects/{id}` returns one project. `PATCH /api/projects/{id}` updates
only supplied settings and returns the saved project. It uses the same validation
as creation, accepts slug changes, and requires at least one field. Omitted fields
stay unchanged; empty strings and zero ports fail validation. Updates preserve the
ID and creation time, and refresh `updated_at`. Concurrent edits to different
fields are preserved; edits to the same field use the last successful write.

`DELETE /api/projects/{id}` removes the project configuration and returns `204`
with no body. Malformed UUIDs return `400`; missing projects return `404`, including
repeated deletes. A project with deployment history returns `409` and stays intact.
These endpoints manage configuration; they do not stop or remove containers.

Project endpoints accept loopback Host values and reject cross-origin writes.
They have no CORS support. These local checks are not admin authentication;
protected access is required before exposing them remotely.

Open `/projects` in the local web app to view the directory. The form at
`/projects/new` saves the six required settings and opens the new project's detail
page. The directory loads 20 records at a time and can refresh or load more.
Validation errors keep the form values intact. Creation is never retried
automatically; after an uncertain response, check the directory before resubmitting.

These screens call guarded Next.js routes for listing, creation, and detail reads.
Both the incoming host and configured API origin must be loopback addresses.
Project routes are disabled whenever Vercel's system variable is present, even
if `SHIPYARD_API_URL` points to a remote HTTPS API. The browser sends no database
credentials, and the gateway does not forward browser cookies or authorization.
Editing and deletion remain API-only for now.

## Deployment attempts

`POST /api/projects/{id}/deployments` creates a `QUEUED` attempt and returns its
saved record (`201`) with a `Location` pointing to `/api/deployments/{id}`. Send no
body, or an empty JSON object with `Content-Type: application/json`. Settings and
status cannot be supplied by the client; they come from the saved project.

Each attempt snapshots the Git URL, requested branch, application port, and health
path in one database operation. Later project edits leave that attempt unchanged.
The branch is still a requested name when queued. One unfinished attempt is allowed
per project, across `QUEUED`, `FETCHING_SOURCE`, `BUILDING`, `STARTING`, and
`HEALTH_CHECKING`, including concurrent requests. Another returns `409`. Failed
attempts remain in history and allow a new attempt.

`GET /api/projects/{id}/deployments` returns `deployments` and `next_offset`, using
the same limit and offset rules as the project directory. An existing project with
no attempts returns an empty list; a missing project returns `404`.
`GET /api/deployments/{id}` returns one saved attempt. Reads and writes use the
same local access checks and three-second database deadline as project endpoints.
Storage failures return a sanitized, uncached `503`.
Records include nullable `commit_sha`, `image_reference`, `image_id`, `instance_id`,
`failure_stage`, `failure_reason`, `started_at`, and `finished_at`. Claim tokens
are private and never returned.

Internal storage methods can claim the oldest available queued attempt, record
its exact source commit, and mark a source failure. Claims use row locks and an
ownership token; a stale token cannot write results. Commit recording is
write-once, with harmless retries of the same SHA. A source failure records a
bounded public reason and finish time, clears ownership, and keeps any recorded
commit. Failed attempts cannot be claimed again.

The worker is disabled by default. When enabled, it claims queued attempts without
blocking HTTP requests, checks out approved source, records its exact commit, and
builds the root Dockerfile through BuildKit. Success saves the image and an
unobserved instance intent in `STARTING`. The checkout stays available throughout
the build and is then removed. Container creation, readiness, and routing are not
connected yet; the unfinished-attempt slot stays occupied at this handoff.

Internal transitions require the claim token and the expected stage. Building
requires a recorded source commit. Starting records the build's unique image tag,
immutable image ID, and a generated runtime instance identity in one transaction,
before Docker creation. Same-stage retries reuse this identity and cannot replace
the image. The database keeps the project's unfinished-attempt slot occupied
through readiness checks.

Runtime records distinguish an unobserved instance intent from observed Docker
state. Observations pin the first full container ID and retain process status,
running flag, exit code, OOM flag, published port, and observation time. Readiness
checks can begin only after a running candidate has been recorded; that stage
does not mean the app is healthy or routed. Failures preserve build and instance
history, clear the claim, and release the unfinished-attempt slot. Marking failure
does not clean up Docker resources. Runtime records are internal for now; desired
state, events, routing, and container recovery remain separate work.

There is no deployment button or frontend gateway for these endpoints yet. Run
migrations before starting the updated API; execution migrations refuse to
downgrade if that would discard source, build, or runtime data.

## Deployment worker

To opt in, set `SHIPYARD_EXECUTOR_ENABLED=true`, `SHIPYARD_WORKSPACE_ROOT` to an
absolute private directory (0700), and `SHIPYARD_ALLOWED_REPOSITORIES` to a
nonempty JSON array of exact approved HTTPS URLs, such as
`["https://github.com/your-account/demo.git"]`. Set `SHIPYARD_DOCKER_HOST` to the
explicit Unix socket URL for the local Docker engine, such as
`unix:///var/run/docker.sock`, before starting the API. Ambient `DOCKER_HOST` and
the CLI context are ignored. Approval is separate from saved project settings;
unapproved attempts fail before network
checkout. Source fetching keeps its two-minute default timeout; builds use five
minutes. Build failures save a fixed public reason and retain the source commit.

One worker polls every second and processes one attempt at a time. It holds a
dedicated PostgreSQL session advisory lock, so a second enabled API process cannot
recover or claim its work. This requires a direct PostgreSQL connection and a
pool with at least two connections. A one-second heartbeat with a three-second
deadline cancels source and build work if ownership is lost. Worker failures stop
the API; shutdown cancels and joins the worker before releasing its session and closing
the database pool.

After acquiring ownership, startup recovers interrupted source/build attempts.
Each worker build uses its persisted deployment UUID as a stable build identity,
so recovery can cancel that exact job and verify/remove its exact image tag.
An interrupted `STARTING` attempt is cleaned only when its linked instance has no
stored Docker observation and inspection confirms that no container exists.
Ownership mismatches, cleanup uncertainty, observed containers, and later stages
stop startup for inspection. Queued attempts stay queued; cleaned interrupted
attempts fail with their metadata preserved. Uncertain database writes halt the
worker and retain the image for recovery rather than guessing whether it was saved.

Recovery does not refetch moving branches or resume removed checkouts. Graceful
shutdown leaves active claims for the next startup. Abrupt crashes can leave
private source directories; remove those only while the worker is stopped.
Docker build cancellation does not wait for every daemon-side action to finish:
a very late exported image can survive the bounded cleanup. Its stable identity
remains derivable from deployment history. Build cache is retained; container
recovery is still pending.

## Source checkout

The Go source module fetches one branch into a private temporary workspace and
returns its exact commit SHA and detached checkout. It requires Git 2.39 or newer
on macOS or Linux, linked with libcurl 7.59 or newer; use a patched Git installation.
The caller supplies an explicit list of approved repository URLs and closes the
checkout when finished. Only an exact URL match is allowed. The optional worker
connects checkout to queued attempts and persists the commit; the source module
itself does not change deployment records.
The workspace root must have private permissions and belong to the service user.
Place it beneath a stable parent controlled by that user, rather than an arbitrary
shared directory. Filesystem permissions do not isolate processes sharing a UID.

Fetches use HTTPS with certificate verification. Repository hosts must resolve
only to public addresses, which are pinned for the fetch. Redirects, credentials,
inherited Git configuration, hooks, and submodule fetching are disabled. Branches
are fetched as `refs/heads/<name>`; tags and revision expressions cannot substitute
for the requested branch. The default deadline is two minutes, covering resolution,
fetch, and checkout. Cancellation stops Git and its transport helpers; failures
remove partial workspaces. Successful workspaces remain until the caller closes
them. An abrupt process or machine crash can leave workspaces behind; startup
cleanup belongs with the executor.

Source handling is for approved, controlled repositories. Shallow fetches still
have no disk quota, and this is not isolation for hostile repositories. Submodule
contents and Git LFS downloads are not included. Tracked symlinks remain symlinks;
build contexts preserve them without reading their targets.
Git command failures retain at most 4 KiB of stderr for private diagnostic logging;
their public error text omits that output. Storage logs include failure categories
and SQLSTATE codes when available, without raw connection or query details.

## Image builds

The Go build module sends a controlled checkout to Docker's embedded BuildKit
through the Docker Go SDK. It requires a regular root `Dockerfile` and an explicit
local Unix socket URL. It does not read Docker CLI credentials, environment
configuration, or the active CLI context. No separate BuildKit service or registry
is needed: the image is exported into that Docker engine's image store and
inspected there before success is returned. Results contain a unique tag and
Docker image ID, with labels identifying the attempt, commit, and build.
Independent builds use fresh identities by default. The worker supplies its
deployment UUID as a stable identity; an existing reserved tag is rejected before
building, and retrying requires a new deployment attempt.

Contexts honor `.dockerignore`, with `Dockerfile.dockerignore` taking precedence.
Git metadata is always excluded. Symlinks are archived as links; their targets
are not read. Dockerfiles and ignore files must be regular files. A private
temporary archive is limited to 256 MiB and 10,000 visited entries; ignore files
are limited to 1 MiB. The archive is removed after use. The caller keeps ownership
of the checkout until the build finishes and of the image after success.

The default five-minute deadline covers preparation, upload, build, and inspection;
the maximum is thirty minutes. Build responses are capped at 16 MiB, and private
failure diagnostics at 4 KiB. HTTP success alone does not mean a build succeeded:
stream errors, incomplete output, and mismatched image identity or ownership fail
the build. Failure requests cancellation and attempts to remove only that build's
unique image tag within five more seconds. BuildKit cache can remain; daemon
failures or abrupt process termination can leave resources for later cleanup.

Docker access is privileged. Builds are for approved, controlled repositories;
this is not isolation for hostile Dockerfiles. Build steps can access the network,
and this module does not enforce CPU, memory, PID, or disk quotas on BuildKit.
Registry authentication, build secrets, custom Dockerfile paths, and multi-platform
builds are not exposed. The optional worker connects this module to queued attempts
and persists the returned image; the build module itself does not start containers
or change deployment records.

## Container runtime

The Go runtime module creates, starts, inspects, stops, and removes containers
through the Docker SDK. It accepts an immutable managed image ID and explicit
project, deployment, and instance identities. Stable instance names make create
retries idempotent; conflicting ownership is rejected. Mutations verify ownership
and use the full container ID. Images stay owned by the caller.

Startup requires Linux Docker Engine 28 or newer and an explicit Unix socket.
The configured application TCP port gets a random port on `127.0.0.1` of the Docker
host; other exposed ports stay unpublished. Docker documents a
[loopback publishing issue in older engines](https://docs.docker.com/engine/network/port-publishing/).
This is a local address, not a stable routed application URL. With Colima, the
Docker host is its Linux VM.

Containers get one CPU, 256 MiB memory without additional swap, a 64-process limit,
all capabilities dropped, and no new privileges. Logs use the local driver with
10 MiB rotation and three files. No host mounts or devices are supplied; images
declaring volumes are rejected. Docker restart policies and inherited health
checks are disabled. Image commands, environment, user, and writable filesystem
are preserved. These controls are for approved applications, not hostile-code
isolation; network access and filesystem disk usage remain unrestricted.

Operations default to thirty seconds, with a two-minute maximum. Stop uses
SIGTERM and a five-second grace period. Failed startup attempts remove only
verified non-running containers; an ambiguous error can leave a running container
for inspection. Removal never forces a running container to stop. Create warnings
fail the operation rather than silently accepting unsupported limits. Private
error diagnostics are bounded to 4 KiB.

Returned state describes Docker's observed process, including exit code and OOM
status. It does not establish HTTP readiness. This module is not connected to the
queue, database lifecycle, routing, or frontend yet. Abrupt crashes and uncertain
create responses can leave owned containers; retry and recovery belong with the
worker.

## Vercel

Import this repository into Vercel with these settings:

- Root Directory: `apps/web`. Enable **Include source files outside of the Root
  Directory in the Build Step** so the workspace files are available.
- Framework Preset: **Next.js**. Use **Node.js 24.x** and leave Output Directory
  at its default. `apps/web/vercel.json` supplies the install and build commands.
- Set `ENABLE_EXPERIMENTAL_COREPACK=1` for Preview and Production to use the pinned
  pnpm version. Keep **System Environment Variables** enabled.

These follow Vercel's [monorepo setup](https://vercel.com/docs/monorepos) and
[Corepack setup](https://vercel.com/docs/builds/configure-a-build#corepack).

Leave `SHIPYARD_API_URL` unset until the Linux API has a reachable HTTPS origin.
The frontend still builds and loads; its health check returns `503` and shows
unavailable. Vercel cannot reach the API running on your computer's loopback
address. When the Linux host is ready, set its HTTPS origin for each deployed
environment and redeploy. With Vercel's `VERCEL=1` system variable, the health
proxy rejects HTTP origins. Keep this setting server-only, without `NEXT_PUBLIC_`.

The Go process stays on the Linux host, behind an HTTPS reverse proxy. Its current
health endpoint only reports liveness. Protected single-admin access must be in
place before adding remotely accessible project, deployment, or log endpoints.
The health proxy does not provide authentication. A Vercel cloud build and the
Linux host setup have not been verified yet.

## Checks

Run `pnpm check` for formatting, lint, and TypeScript checks, `pnpm test` for the
health and project integration tests, and `pnpm build` for the production frontend build.
The build does not require a running API. Run `pnpm format` to apply formatting.

From `apps/api`, run `gofmt -l .`, `go vet ./...`, `go test ./...`,
`go test -race ./...`, and `go build ./...`. Tests cover API configuration, health
responses, routing, shutdown, project validation and HTTP behavior, and the
migration CLI, deployment request handling, history-preserving deletion, and
source input and network policy.
Ordinary unit tests use a stub Git executable and do not require an installed Git.
The API builds on other operating systems, but source checkout returns an explicit
unsupported-platform error there.

Database tests are opt-in. From `apps/api`, run
`SHIPYARD_TEST_DATABASE_URL="$SHIPYARD_DATABASE_URL" go test -tags=integration ./...`
against local PostgreSQL. Tests create and remove isolated schemas; the test role
must be allowed to create schemas. They check migration reruns, preserved records,
schema constraints, concurrent migration commands, project persistence, duplicate
slugs, partial and concurrent updates, deletion, pagination order, and canceled
queries. Deployment tests check saved snapshots, concurrent queueing and claiming,
claim ownership, exact commit persistence, terminal failures, history pagination,
project deletion races, and migration upgrades and downgrade safety.

With the supported Git installation and `git-http-backend` available, the
integration suite also uses a local HTTPS Git server to check exact branch
commits, detached checkouts, configuration isolation, and
workspace cleanup. It does not need public-network access. To additionally fetch
an approved public repository, set `SHIPYARD_TEST_GIT_URL` and optionally
`SHIPYARD_TEST_GIT_BRANCH` (default `main`) when running the integration suite.
Set `SHIPYARD_TEST_GIT_COMMIT` to assert a particular expected commit. Source
integration tests require macOS or Linux and fail if their Git dependency is missing.

To test image builds, set `SHIPYARD_TEST_DOCKER_HOST` to an explicit Unix socket
URL for a disposable Linux Docker engine when running the integration suite.
These tests use pull-free `FROM scratch` fixtures, verify local image availability,
exercise real build failures and ignore rules, and remove their own image tags.
They skip when the variable is unset and fail if the configured engine is
unavailable or lacks BuildKit. Ordinary unit tests do not require Docker.

The same Docker test setting enables runtime integration tests. They build a
small static Go HTTP fixture, exercise create/start/inspect/stop/remove, and check
actual cgroup v2 resource limits, loopback port mapping, ownership, and failed-start
cleanup. These checks require cgroup v2 and HTTP access to the Docker host's
loopback. For a VM, `SHIPYARD_TEST_DOCKER_HTTP_PROXY` can point to a temporary SSH
SOCKS5 tunnel at `socks5://127.0.0.1:port`. It affects test HTTP requests only.
Fixture binaries live in temporary test directories. Unit tests use a fake engine.

Executor integration tests use isolated PostgreSQL schemas and blocked source or
build operations to verify responsive API requests, exclusive ownership, and
restart recovery. With `SHIPYARD_TEST_DOCKER_HOST`, controlled scratch fixtures
exercise real queued builds, saved images, build failures, and guarded cleanup.
Set `SHIPYARD_TEST_EXECUTOR_GIT_URL` to an explicitly approved public HTTPS repo to
also test real checkout through a queued API request; the branch defaults to
`main` and can be set with `SHIPYARD_TEST_EXECUTOR_GIT_BRANCH`. That optional test
blocks a fake builder to verify that the real checkout remains available during
building and is removed on cancellation; it does not require a Dockerfile or run
that repository's build.
