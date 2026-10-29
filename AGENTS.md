# Model Meter: instructions for installation agents

Use this guide when asked to install or update Model Meter. Read README.md for configuration and supported API routes. This is a Go service with an embedded web UI and SQLite; it does not require a frontend build or Node.js to run.

## Inspect the target

- Honor the user's requested checkout location, upstream, port, and network access. Defaults are `$HOME/model-meter`, upstream `http://127.0.0.1:8001`, and proxy port 8000.
- Check existing files, Git changes, service configuration, and listening ports before changing anything. Reuse an existing installation without replacing its data or credentials. Do not stop or reconfigure the upstream model server unless the user explicitly requests it.
- Check `go version`, `go env CGO_ENABLED`, and a C compiler such as `cc`. Use a Go toolchain satisfying `go.mod`. The SQLite driver requires CGO and a working C toolchain.
- The supplied service targets Linux with user systemd. On other systems, build and run the binary under an available process manager; report the actual persistence configuration.

## Clone and build

For a new installation at the default location:

```sh
git clone https://github.com/ukd1/model-meter.git "$HOME/model-meter"
cd "$HOME/model-meter"
go test ./...
go vet ./...
go build -o model-meter .
```

For an update, inspect the existing branch and changes first; do not discard local work. Build a replacement binary before restarting. Never reset or delete `data/` to resolve a deployment issue.

## Configure and start on Linux

Run from the project directory:

```sh
mkdir -p ./data "$HOME/.config/systemd/user"
chmod 700 ./data
cp ./deploy/model-meter.service "$HOME/.config/systemd/user/model-meter.service"
systemd-analyze --user verify "$HOME/.config/systemd/user/model-meter.service"
systemctl --user daemon-reload
systemctl --user enable --now model-meter
```

Inspect an existing installed unit before copying over it. For a checkout outside `$HOME/model-meter`, adjust WorkingDirectory, ExecStart, DATA_DIR, EnvironmentFile, and ReadWritePaths in the installed unit. `%h` means the user's home directory in systemd; `$HOME` is for shell commands. Do not commit a machine-specific unit back into the repository.

The optional `data/service.env` file can set `LISTEN_ADDR`, `UPSTREAM_URL`, and `UPSTREAM_API_KEY`. Create it with mode 0600 and preserve existing settings. Set the upstream to its origin without a trailing `/v1`. A loopback-only bind is `LISTEN_ADDR=127.0.0.1:8000`; `:8000` listens on all interfaces. Match the user's access requirements. If changing DATA_DIR, also update the unit's writable directory and preserve existing data.

After changing a running installation, use `systemctl --user restart model-meter`. Check `loginctl show-user "$USER" -p Linger` if startup without an interactive login is required. Do not claim boot persistence without verifying the host's user-service lifecycle.

## Verify

1. Check `systemctl --user is-active model-meter` and `GET /healthz` on the configured port. Health confirms the proxy/database, not upstream inference.
2. Check that `/` serves the dashboard and unauthenticated `/admin/keys` returns 401. `/v1/models` also returns 401 unless an administrator has enabled anonymous API access.
3. On first setup, retrieve the generated administrator username/password from the startup console or service journal into memory. For an existing installation, use the operator-provided account. Do not put passwords in command arguments, screenshots, source, or commits. Authenticate with JSON username/password at `POST /auth/login`, header `X-Model-Meter: dashboard`, and retain its session cookie privately. The service prints the initial password once; do not reproduce it in reports unless requested.
4. With the session cookie, `Content-Type: application/json`, and `X-Model-Meter: dashboard`, create a temporary key using `POST /admin/keys` and JSON `{"name":"Installation verification"}`. Use the returned `key` for `GET /v1/models` and discover an actual upstream model name.
5. Exercise small regular and streamed inference requests for the upstream's supported chat formats. Inspect response usage transiently and compare it with `GET /admin/requests?key=<id>` after allowing the asynchronous writer to flush. Never persist inference bodies. Missing upstream usage must remain unmetered, not fabricated.
6. Revoke the temporary key with `POST /admin/revoke` and JSON `{"id":<id>}` in cleanup, including after failures. Confirm subsequent API requests with it return 401.
7. Check `/admin/status` for dropped records. For a fresh installation, restart once and verify health, key revocation, and request history persist. Avoid unnecessary restarts of a busy existing service.

Report the dashboard URL, client base URLs, service status, initial login retrieval instructions, verification results, and any checks that could not be completed. Do not call installation verified if only the process started.

## Development and privacy constraints

Keep request accounting asynchronous and bounded. Preserve per-user key/usage isolation. Never log request or response bodies, arbitrary headers, or URL query strings. Do not add credential logging; the initial bootstrap password is printed once by the service for setup. Use username/password sessions for dashboard access. `/v1` requires separate API keys by default; administrators can opt into anonymous API access. Anonymous requests must remain visible in administrator usage totals and isolated from regular users. Invalid supplied keys must still be rejected. Keep missing usage distinguishable from zero. Match streaming usage semantics rather than summing cumulative chunks. Test protocol changes, including streaming and credential handling, before deployment. Runtime data and binaries must remain ignored by Git.

For password recovery, run `./model-meter reset-password USERNAME` with the service's `DATA_DIR`. It generates a password shown once and revokes that account's sessions. Do not reset an existing password during routine verification. There is no master administrator token or email reset flow.

For upgrades from token sign-in, preserve existing user IDs, API keys, and usage. Migrated accounts use `user-ID` usernames and require a password reset before dashboard login; administrators can then change their usernames. Existing proxy API keys remain valid. Do not recreate accounts as a migration shortcut.
