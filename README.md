# sap-attachment-api

File ingestion service for SAP Business One: accepts PDF/image attachments by
direct upload or remote source (HTTP URL or `s3://bucket/key`), stores them on
local disk or an SMB share, and returns metadata including `storage_path` for
SAP to read the file.

## Quick start

```sh
cp .env.example .env      # edit as needed
make run                  # listens on :8080
```

Prebuilt binaries from GitHub Releases are self-contained — **configuration is
never baked in**. At startup the service reads `./.env`, or an explicit file:

```sh
./sap-attachment-api --config /etc/sap/.env    # or CONFIG_FILE=/etc/sap/.env
# Windows: sap-attachment-api.exe --config C:\sap\.env
```

Real environment variables override the file. Set `PORT=9090` (or
`ADDR=:9090`) in it to change the listen port. Health: `/healthz`, readiness:
`/readyz`, docs: `/docs`, contract: `/openapi.yaml`. All variables are listed
in `.env.example`.

## API

Every `/v1` endpoint needs `X-API-Key`; every POST needs a UUID
`X-Idempotency-Key` (missing → `400`). Errors use `{"code","message","request_id"}`.

| Method | Path                       | Purpose                                          |
| ------ | -------------------------- | ------------------------------------------------ |
| POST   | `/v1/attachments`          | Multipart `file` part, or JSON `source_url` / `source_uri` |
| GET    | `/v1/attachments/{id}`     | Metadata                                         |

Only PDF and image bytes are accepted (sniffed from content, not the filename);
anything else returns `415`. Duplicate content returns the original record
(`200` instead of `201`). The idempotency key becomes the stored filename
(`<key>.<ext>`) so Windows paths stay short, and retries replay the first
response.

```sh
curl -X POST localhost:8080/v1/attachments \
  -H "X-API-Key: local-dev-key" -H "X-Idempotency-Key: $(uuidgen)" \
  -F file=@invoice.pdf
```

## Storage

- `STORAGE_BACKEND=local` (default): files under `STORAGE_DIR`; `storage_path`
  is an absolute disk path.
- `STORAGE_BACKEND=smb`: the service speaks SMB2/3 directly, no OS mount.
  Set `SMB_SERVER`, `SMB_SHARE`, optional `SMB_BASE_DIR` (folder inside the
  share) and NTLMv2 credentials (`SMB_USERNAME`/`SMB_PASSWORD`, optional
  `SMB_DOMAIN`). `storage_path` is a Windows UNC path. Commits are staged in
  the share and renamed into place; SAP must be able to reach the share.
- SQLite (`DB_PATH`) and upload scratch space (`STAGING_DIR`) are **always
  local**, even with the SMB backend.

See `.env.example` for the full option list, including HTTP/S3 fetch settings.

## SAP attachment entries (optional)

Set all four of `SAP_DATABASE`, `SAP_SERVICE_LAYER_URL`, `SAP_USERNAME` and
`SAP_PASSWORD` to register each newly stored file in SAP through Service
Layer: log in (`/b1s/v1/Login`), then `POST /b1s/v1/Attachments2` with
`SourcePath` = the stored file's directory (`\\IP\nas\sales`), `FileName` =
the stored key and `FileExtension` = its extension. The session is kept warm:
it is refreshed every 25 minutes and reused between uploads. SAP must be able to read
that directory. A duplicate (deduplicated) upload creates no new entry. If
the SAP call fails, the API returns `502` but keeps the file and record; a
later upload of the same content retries only the SAP entry. Responses
include `sap_absolute_entry` (SAP's `AbsoluteEntry` id) once registered.
Use `SAP_INSECURE_SKIP_VERIFY=true` only for self-signed SAP certificates.

## Development and releases

```sh
make check       # fmt, vet, lint, race tests, build, govulncheck
make build
make docker-build
```

CI runs `make check` on push and PR (also manually dispatchable). Pushing a
`v*` tag — or dispatching Release with a version — reruns CI, then builds
binaries for Linux (amd64/arm64), Windows (amd64) and macOS (amd64/arm64) and
publishes them with SHA-256 checksums and `.env.example` in a GitHub Release.
