# Shipyard

Shipyard is a single-host deployment platform in development. So far it has a Go
health API and a Next.js shell with dark and light themes. The Overview checks
whether the Go API is responding. Projects and application deployment come next.

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

Open http://127.0.0.1:3000. To run the API in another terminal:

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
health integration tests, and `pnpm build` for the production frontend build.
The build does not require a running API. Run `pnpm format` to apply formatting.

From `apps/api`, run `gofmt -l .`, `go vet ./...`, `go test ./...`,
`go test -race ./...`, and `go build ./...`. The API uses only the Go standard
library. Its tests cover configuration, health responses, routing, and shutdown.
