# Local routing

The routing module publishes one Traefik file-provider configuration per project.
It is tested separately from the deployment worker. Deployments still stop at
`HEALTH_CHECKING`; automatic activation and saved active-route state come next.

Run Traefik beside Go on the Linux Docker host. Both need access to the app ports
published on that host's `127.0.0.1`. A native process is the simplest setup. A
Linux host-network container also works, but a normal bridge container has its
own loopback and cannot reach those ports directly.

## Start Traefik

Install a native Traefik v3 binary from its
[official releases](https://github.com/traefik/traefik/releases). This adapter was
verified with v3.7.16. Create a dedicated directory owned by the service user:

```sh
export SHIPYARD_ROUTES_DIR="$HOME/.local/share/shipyard/routes"
install -d -m 0700 "$SHIPYARD_ROUTES_DIR"
traefik --entrypoints.web.address=127.0.0.1:8180 \
  --providers.file.directory="$SHIPYARD_ROUTES_DIR" --providers.file.watch=true \
  --providers.docker=false --api=false \
  --global.checknewversion=false --global.sendanonymoususage=false
```

This listener is local HTTP only. The Docker provider and dashboard stay disabled;
Traefik does not need the Docker socket. Keep the directory beneath a stable
parent controlled by the service user. When containerizing Traefik later, mount
the whole directory read-only, rather than binding individual generated files.

## Route publication

`routing.Manager` accepts an already verified container binding: project,
deployment, instance, full Docker container ID, and observed host port. It uses
`<project-uuid>.shipyard.localhost` so editing a project's slug does not change its
hostname. It forwards requests to that container's loopback port, preserving the
incoming host, path, and query.

Publishing requires either an absent route or the exact expected previous
binding. Removal also requires an exact match. Generated TOML files contain an
ownership header; edited, foreign, oversized, or symlinked files are refused.
Keep unrelated Traefik configurations outside this dedicated directory.

Writers share a local filesystem lock. Publication writes and syncs a private
temporary file, renames it atomically, then syncs the directory. Errors after a
rename or deletion can leave the change published; inspect the current binding
before retrying. Calls honor cancellation while waiting for ownership and before
mutation, but cannot interrupt an in-progress local filesystem syscall.

A successful write means the config is saved. Traefik reloads asynchronously;
the future activation step must confirm actual routing before reporting success.
The adapter itself does not probe readiness, change PostgreSQL, or retire apps.

## Local access and verification

After a route is published, its URL is
`http://<project-uuid>.shipyard.localhost:8180/`. An empty directory serves no app
routes. If local DNS does not resolve the hostname to loopback, use an explicit
hosts entry or `curl --resolve '<hostname>:8180:127.0.0.1' 'http://<hostname>:8180/'`.

Set `SHIPYARD_TEST_TRAEFIK_BINARY` to an absolute native executable path when
running `go test -race -tags=integration ./internal/routing`. Tests verify actual
HTTP routing, hostname isolation, replacement, stale cleanup, removal, and
configuration loading after a Traefik restart. They use explicit loopback
connections, independently of local DNS.

On macOS with Colima, the Docker host is the Linux VM. Native Mac Traefik cannot
directly reach the VM's loopback ports. The routing tests use local HTTP fixtures;
full Docker-app routing still needs the components running together inside the
VM or an explicit development tunnel.
