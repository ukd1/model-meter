# Model Meter

Go gateway and dashboard for an OpenAI-compatible upstream. Both run on **port 8001**; the upstream defaults to **http://127.0.0.1:8000**. SQLite stores users, hashed credentials, and request metadata.

## Use

Open `http://<server>:8001`. Read the initial administrator sign-in token with:

```sh
cat ./data/admin-token
```

The administrator creates users and receives each user's sign-in token once. Users sign in to the same page and can create, rename, and revoke their own API keys, change their display name, and inspect their usage. The administrator can manage all accounts, reset sign-in tokens, disable users, and filter aggregate usage by user, key, model, and period. Disabling a user blocks both their sign-in token and all their API keys. User tokens and API keys are separate credentials. Full credentials are never returned after creation; only hashes and API key prefixes are stored. Administrator credentials reside in a mode-0600 file. Dashboard credentials stay in browser memory until the tab is refreshed or locked.

Configure your OpenAI-compatible client:

```sh
export OPENAI_BASE_URL=http://<server>:8001/v1
export OPENAI_API_KEY=mm_<key-created-in-dashboard>
curl "$OPENAI_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Hello"}],"max_tokens":32}'
```

Supported routes: `GET /v1/models`, `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`, and `/v1/responses`. Upstream support determines which routes work. Streaming is forwarded incrementally. For streaming chat/completions the gateway sets `stream_options.include_usage=true`. Other request JSON values are preserved semantically. Usage extraction supports chat/completions, embeddings, and Responses JSON/SSE formats. API errors retain upstream status and response body.

## Accounting and privacy

Persisted request fields: key ID, UTC start time, model, method, known endpoint label, direct peer IP, HTTP status, total duration in milliseconds, input/output/cached tokens, and completion/interruption outcome. No request/response bodies, arbitrary headers, authorization values, or query strings are logged. Bodies are inspected transiently in memory to extract model and usage. Model names and administrator/user supplied names are intentional metadata. Client keys are replaced with the optional upstream credential; client headers are not blindly forwarded.

Completed metadata enters a bounded **8192-record queue without waiting**. A background writer commits SQLite batches every 250 ms or 100 records. WAL with a connection pool lets authentication reads run alongside accounting writes. Graceful shutdown drains handlers and the queue. Crashes can lose queued records; a full queue or failed transaction drops accounting records instead of blocking responses. The dashboard warns about dropped records, and `/admin/status` exposes `dropped_records` and `queued_records` (dropped count resets on restart). This is usage monitoring, not billing-grade accounting.

Totals use upstream-reported usage. Missing usage is NULL and shown as unmetered, never estimated. Interrupted streams may lack final usage. Cached tokens are included in input totals. Metadata persists indefinitely; back up SQLite with its backup API or while the service is stopped. The live charts show one-minute buckets for the past hour, refresh every 15 seconds, and honor selected filters. Recent requests are limited to the latest 200; aggregates cover all matching history.

Request bodies are limited to 32 MiB; metering inspection buffers are limited to 8 MiB per JSON response/SSE line. Oversized or unrecognized usage envelopes are unmetered. Requests have a 30-minute upstream deadline. The service records authenticated requests; rejected authentication is not attributed to a key. Existing in-flight requests can finish after a key is revoked.

## Operate

```sh
go test -race ./...
go vet ./...
go build -o model-meter .
systemctl --user status model-meter
systemctl --user restart model-meter
journalctl --user -u model-meter
```

The installed unit is in `~/.config/systemd/user/model-meter.service`; the repository copy is `deploy/model-meter.service`. Configuration: `LISTEN_ADDR`, `UPSTREAM_URL` (origin without `/v1`), `DATA_DIR`, and optional `UPSTREAM_API_KEY`. Service overrides can be placed in ignored `data/service.env` with mode 0600. Upstream credentials are not managed in the UI: its keys identify callers of this gateway.

The local deployment uses HTTP, like the existing upstream. Use a trusted network or SSH tunnel (`ssh -L 8001:localhost:8001 <user>@<server>`) for access; terminate TLS before exposing it to an untrusted network. Forwarded IP headers are deliberately not trusted. No external frontend dependencies, fonts, or analytics are loaded.
