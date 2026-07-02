# ── build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# modernc.org/sqlite is pure Go, so the binary is fully static (no CGO).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/katana ./cmd/katana

# ── runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates is needed for HTTPS calls to gnomonitoring and gno.land RPC.
RUN apk add --no-cache ca-certificates \
	&& adduser -D -u 10001 katana

WORKDIR /app

# Data dir for the SQLite database and dry-run proposals. Created with the right
# ownership so a fresh named volume mounted here is writable by the katana user.
RUN mkdir -p /app/data && chown -R katana:katana /app

COPY --from=build /out/katana /usr/local/bin/katana

USER katana

# katana is a long-running daemon; it loops internally until it receives a
# termination signal. No wrapper/sleep loop needed.
ENTRYPOINT ["katana"]
CMD ["--config", "/app/config.yaml"]
