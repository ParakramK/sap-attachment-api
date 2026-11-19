# sap-attachment-api

Production-grade file ingestion service for internal SAP Business One
integration: receives attachments as direct uploads or by downloading a
remote source (HTTP URL or S3 object), stores them on the local filesystem
or an SMB share with content checksums, and exposes metadata over a
documented HTTP API. SAP reads the stored files directly from the
`storage_path` in the response.

## Stack

- Go 1.27, [Fiber v3](https://docs.gofiber.io)
- SQLite (WAL) via [Bun](https://bun.uptrace.dev) + pure-Go `modernc.org/sqlite`
- Local filesystem storage (atomic staged writes), or a remote SMB share
  spoken directly by the service (SMB2/3, no OS mount)
- Structured JSON logging (`slog`), optional Better Stack remote sink
- OpenAPI 3 contract served at `/openapi.yaml`, interactive docs at `/docs`

## Running

```sh
cp .env.example .env   # optional: tune via environment variables
make run               # dev mode on :8080
```

`.env` is loaded at startup (real environment variables win over it). In
Docker, pass variables directly — `.env` is not bundled into the image.

Health: `GET /healthz` · Readiness: `GET /readyz` · Docs: `GET /docs`

## API

All `/v1` endpoints require `X-API-Key` (set `API_KEYS`; unset keys disable
auth in dev). Errors use one envelope: `{"code", "message", "request_id"}`.

### Upload a file directly

Only **PDF and image** content is accepted. The type is sniffed from the
file bytes server-side; other formats are rejected with `415`.

```sh
curl -X POST http://localhost:8080/v1/attachments \
  -H "X-API-Key: local-dev-key" -H "X-Idempotency-Key: $(uuidgen)" \
  -F "file=@invoice.pdf"
```

### Ingest from a remote source

```sh
curl -X POST http://localhost:8080/v1/attachments \
  -H "X-API-Key: local-dev-key" -H "X-Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" \
  -d '{"source_url": "https://example.com/report.pdf"}'

curl -X POST http://localhost:8080/v1/attachments \
  -H "X-API-Key: local-dev-key" -H "X-Idempotency-Key: $(uuidgen)" -H "Content-Type: application/json" \
  -d '{"source_uri": "s3://my-bucket/invoices/2026-01.pdf"}'
```

Remote fetches are SSRF-hardened: private/reserved network targets are
denied unless `FETCH_ALLOW_PRIVATE_NET=true`, redirects are bounded and
re-validated, and only http(s)/s3 schemes are accepted.

### Metadata, dedupe, idempotency

`GET /v1/attachments/{id}` returns the metadata record; `DELETE /v1/
attachments/{id}` removes the stored file and its record (the only
retention mechanism — no automatic expiry).

- Identical content is stored once: re-uploads return the original record
  with `200` instead of `201` (dedupe key: SHA-256).
- Every POST must carry `X-Idempotency-Key` (UUID): the original response is
  replayed on retries instead of re-uploading. Keys are scoped per API key.
  Requests without a valid key are rejected with `400`. The key also becomes
  the stored file name (`<key>.<original extension>`) — the client's filename
  is never used as a path, only its short extension, to keep Windows paths
  within limits.

## Storage backends

`STORAGE_BACKEND=local` (default) writes to a local directory; `storage_path`
in API responses is the absolute disk path SAP reads.

`STORAGE_BACKEND=smb` speaks SMB2/3 directly — no mount needed. The service
signs in with a service account (NTLMv2), stages each upload in a staging
folder inside the share and atomically renames it into place; `storage_path`
becomes the Windows UNC path (`\\server\share\<base-dir>\<idempotency-key>.pdf`). The SAP side
must itself have access to the same share. SQLite (`DB_PATH`) and the upload
scratch directory (`STAGING_DIR`) always stay on local disk.

```sh
STORAGE_BACKEND=smb
SMB_SERVER=192.168.1.10        # or host:port
SMB_SHARE=nas
SMB_BASE_DIR=attachments   # writes to \\192.168.1.10\nas\attachments
SMB_USERNAME=sap-attachments
SMB_PASSWORD=...               # secret
SMB_DOMAIN=CORP                # optional
```

## Configuration (environment)

| Variable               | Default                  | Description                                    |
| ---------------------- | ------------------------ | ---------------------------------------------- |
| `APP_ENV`              | `dev`                    | `dev` or `prod` (prod requires API_KEYS)       |
| `ADDR`                 | `:8080`                  | Listen address                                 |
| `LOG_LEVEL`            | `info`                   | slog level for stdout                          |
| `LOGTAIL_TOKEN`        | –                        | Better Stack source token (secret)             |
| `LOGTAIL_LEVEL`        | `info`                   | Minimum level shipped to Better Stack          |
| `SQL_DEBUG`            | `false`                  | Verbose SQL logging (very noisy; debug only)   |
| `STORAGE_DIR`          | `./data/storage`         | Attachment storage directory                   |
| `STAGING_DIR`          | `./data/staging`         | Upload staging directory (always local scratch) |
| `DB_PATH`              | `./data/attachments.db`  | SQLite database path — **always local**, also with the SMB backend |
| `STORAGE_BACKEND`      | `local`                  | `local` or `smb` (service speaks SMB directly)  |
| `SMB_SERVER`           | –                        | SMB host or host:port (when backend is smb)    |
| `SMB_SHARE`            | –                        | Share name (e.g. `nas` for `\\IP\nas\attachments`) |
| `SMB_BASE_DIR`         | –                        | Optional folder inside the share (e.g. `attachments`); empty = share root |
| `SMB_USERNAME`         | –                        | Service-account user (NTLMv2)                  |
| `SMB_PASSWORD`         | –                        | Service-account password (secret)              |
| `SMB_DOMAIN`           | –                        | Optional AD domain                             |
| `SMB_REQUIRE_SIGNING`  | `true`                   | Require SMB3 message signing                   |
| `SMB_STAGING_DIR`      | `staging`                | Commit staging dir inside the share            |
| `API_KEYS`             | –                        | Comma-separated API keys (secret)              |
| `MAX_UPLOAD_MB`        | `50`                     | Max upload size in megabytes                   |
| `FETCH_ALLOW_PRIVATE_NET` | `false`               | Allow fetching private/reserved network hosts  |
| `FETCH_TIMEOUT`        | `60s`                    | Budget for one remote fetch (incl. redirects)  |
| `S3_REGION`            | `us-east-1`              | AWS region for s3:// source fetches            |
| `S3_ENDPOINT`          | –                        | Custom endpoint (e.g. MinIO)                   |
| `S3_PATH_STYLE`        | `false`                  | Path-style addressing (true for MinIO)         |
| `S3_ACCESS_KEY_ID`     | –                        | Static key; omit to use AWS default chain      |
| `S3_SECRET_ACCESS_KEY` | –                        | Static secret (secret)                         |
| `S3_ALLOWED_BUCKETS`   | –                        | Comma-separated s3:// bucket allowlist; empty = any readable bucket |
| `SHUTDOWN_TIMEOUT`     | `15s`                    | Graceful shutdown deadline                     |

Secrets are read from the environment only.

## Development

```sh
make check    # fmt + vet + lint + race tests + govulncheck
make test
make build
make docker-build
```
