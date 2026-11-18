# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/sap-attachment-api ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/sap-attachment-api /sap-attachment-api

ENV APP_ENV=prod \
    ADDR=:8080 \
    STORAGE_DIR=/data/storage \
    DB_PATH=/data/attachments.db
# LOGTAIL_TOKEN and API_KEYS are supplied via the environment at runtime.

WORKDIR /
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/sap-attachment-api"]
