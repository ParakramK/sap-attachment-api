# sap-attachment-api

Production-grade file ingestion service for internal SAP Business One
integration: receives attachment uploads (multipart or URL fetch), stores them
on the local filesystem with content checksums, and exposes metadata over a
documented HTTP API.

## Stack

- Go 1.27, [Fiber v3](https://docs.gofiber.io)
- SQLite (WAL) via [Bun](https://bun.uptrace.dev) + pure-Go `modernc.org/sqlite`
- Storage backends: local filesystem (atomic staged writes) or S3/MinIO
- Structured JSON logging (`slog`), optional Better Stack remote sink
- OpenTelemetry tracing, Prometheus metrics, OpenAPI 3 contract (oapi-codegen)

## Running

```sh
cp .env.example .env   # optional: tune via environment variables
make run               # dev mode on :8080
```

Health: `GET /healthz`

## Configuration (environment)

| Variable           | Default              | Description                              |
| ------------------ | -------------------- | ---------------------------------------- |
| `APP_ENV`          | `dev`                | `dev` or `prod` (prod requires API_KEYS) |
| `ADDR`             | `:8080`              | Listen address                           |
| `LOG_LEVEL`        | `info`               | slog level for stdout                    |
| `LOGTAIL_TOKEN`    | –                    | Better Stack source token (secret)       |
| `LOGTAIL_LEVEL`    | `info`               | Minimum level shipped to Better Stack    |
| `STORAGE_DIR`      | `./data/storage`     | Attachment storage directory             |
| `STAGING_DIR`      | `./data/staging`     | Upload staging directory                 |
| `DB_PATH`          | `./data/attachments.db` | SQLite database path                 |
| `STORAGE_BACKEND`  | `local`              | `local` or `s3`                          |
| `S3_BUCKET`        | –                    | Bucket (required when backend is s3)     |
| `S3_REGION`        | `us-east-1`          | AWS region                               |
| `S3_ENDPOINT`      | –                    | Custom endpoint (e.g. MinIO)             |
| `S3_PATH_STYLE`    | `false`              | Path-style addressing (true for MinIO)   |
| `S3_ACCESS_KEY_ID` | –                    | Static key; omit to use AWS default chain |
| `S3_SECRET_ACCESS_KEY` | –                | Static secret (secret)                   |
| `API_KEYS`         | –                    | Comma-separated API keys (secret)        |
| `MAX_UPLOAD_MB`    | `50`                 | Max upload size in megabytes             |
| `SHUTDOWN_TIMEOUT` | `15s`                | Graceful shutdown deadline               |

Secrets are read from the environment only.

## Development

```sh
make check    # fmt + vet + lint + race tests + govulncheck
make test
make build
make docker-build
```
