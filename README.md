# Shipyard

Shipyard is a single-host deployment platform in development. So far it has a Go
API and a Next.js UI with dark and light themes. The Overview checks whether
the Go API is responding. Local project screens let you create a project, browse
the directory, and view its saved settings. The API also supports editing and
deleting configuration. Deployment attempts can be queued and inspected through
the local API. A source checkout module is tested separately; the executor,
image builds, and container startup come next.

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
The branch is still a requested name; no commit has been fetched or resolved yet.
One queued attempt is allowed per project, including concurrent requests; another
returns `409`.

`GET /api/projects/{id}/deployments` returns `deployments` and `next_offset`, using
the same limit and offset rules as the project directory. An existing project with
no attempts returns an empty list; a missing project returns `404`.
`GET /api/deployments/{id}` returns one saved attempt. Reads and writes use the
same local access checks and three-second database deadline as project endpoints.
Storage failures return a sanitized, uncached `503`.

This milestone stores attempts only. There is no executor yet: queued records
survive restarts, stay queued, and block another attempt until execution is added.
There is no deployment button or frontend gateway for these endpoints yet.

## Source checkout

The Go source module fetches one branch into a private temporary workspace and
returns its exact commit SHA and detached checkout. It requires Git 2.39 or newer
on macOS or Linux, linked with libcurl 7.59 or newer; use a patched Git installation.
The caller supplies an explicit list of approved repository URLs and closes the
checkout when finished. Only an exact URL match is allowed. This module is not
connected to queued attempts yet,
so it does not change deployment status or persist the commit.
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
future build-context handling must keep them from exposing host files.
Git command failures retain at most 4 KiB of stderr for private diagnostic logging;
their public error text omits that output. Storage logs include failure categories
and SQLSTATE codes when available, without raw connection or query details.

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
queries. Deployment tests check saved snapshots, concurrent queueing, persistence,
project deletion races, and migration upgrades.

With the supported Git installation and `git-http-backend` available, the
integration suite also uses a local HTTPS Git server to check exact branch
commits, detached checkouts, configuration isolation, and
workspace cleanup. It does not need public-network access. To additionally fetch
an approved public repository, set `SHIPYARD_TEST_GIT_URL` and optionally
`SHIPYARD_TEST_GIT_BRANCH` (default `main`) when running the integration suite.
Set `SHIPYARD_TEST_GIT_COMMIT` to assert a particular expected commit. Source
integration tests require macOS or Linux and fail if their Git dependency is missing.
