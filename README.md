# Shipyard

Shipyard is a deployment platform in development. The current foundation is a
local Go HTTP service with a health endpoint. Application deployment, persistence,
routing, recovery, and the web interface are not implemented yet.

The planned hosting setup is a Next.js frontend on Vercel and a Go deployment
engine on one Linux Docker host. The current service runs locally only.

## Run the API

Requires Go 1.27.2 or newer in the Go 1.27 release series.

```sh
cd apps/api
go run ./cmd/server
```

In another terminal:

```sh
curl --fail --silent --show-error http://127.0.0.1:8080/api/health
```

The response is `200 OK` with `Content-Type: application/json` and:

```json
{"status":"ok"}
```

This reports process liveness only. It does not check external dependencies.
`HEAD /api/health` is also supported; other methods return `405` and unknown paths
return `404`.

The optional `SHIPYARD_API_ADDR` environment variable defaults to
`127.0.0.1:8080`. It accepts a literal loopback IP and a port from 1 to 65535:

```sh
SHIPYARD_API_ADDR=127.0.0.1:9090 go run ./cmd/server
```

IPv6 loopback (`[::1]:8080`) is supported. Empty values, hostnames, wildcard
addresses, non-loopback addresses, and invalid ports are rejected before binding.

Press Ctrl+C or send SIGTERM to stop. The API allows active requests up to five
seconds to complete before closing remaining connections. Startup errors and
shutdown failures produce a nonzero exit status.

## Verify

From `apps/api`:

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
go build ./...
```

`gofmt -l .` should print nothing. Tests cover address validation, the health
contract and routing, active-request draining, and listener failures. The API uses
only the Go standard library.
