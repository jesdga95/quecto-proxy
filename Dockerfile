# Build the dashboard first so it can be embedded into the binary.
FROM node:22-alpine AS dashboard
WORKDIR /src/dashboard
COPY dashboard/package.json dashboard/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY dashboard/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=dashboard /src/internal/web/dist ./internal/web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /quecto ./cmd/quecto
# The data directory is created here so it can be copied in owned by the
# unprivileged user the final image runs as. A named volume mounted over it
# inherits that ownership, so the database is writable without running as root.
RUN mkdir /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /quecto /quecto
COPY LICENSE NOTICE /
COPY --from=build --chown=65532:65532 /data /data

ENV QUECTO_LISTEN=:11312 \
    QUECTO_ADMIN_LISTEN=:11313 \
    QUECTO_DB_PATH=/data/quecto.db
EXPOSE 11312 11313
VOLUME ["/data"]
# Run as a non-root user so a container breakout does not start as uid 0. The
# numeric id needs no /etc/passwd entry, which a scratch image has none of.
USER 65532:65532
ENTRYPOINT ["/quecto"]
