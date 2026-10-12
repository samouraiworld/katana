# Pin to the SAME gno revision the target network was built from. The default is
# the gno.land mainnet: the gnolang/gno `chain/mainnet` tag commit (also tagged
# v1.2.0). Pin the full SHA rather than the tag, since tags can move. For the e2e
# devnet this must match gnoland-test's GNO_REPO_PATH commit; for test13 use
# f45cc5c88e6a (and override the run scripts, see config.yaml.example).
ARG GNO_REF=9c8eb132e483d6fd324d92c193e629ad65a98a37

# ── build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# modernc.org/sqlite is pure Go, so the binary is fully static (no CGO).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/katana ./cmd/katana

# ── gnokey stage ──────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS gnokey
ARG GNO_REF
RUN apk add --no-cache git
RUN CGO_ENABLED=0 go install github.com/gnolang/gno/gno.land/cmd/gnokey@${GNO_REF}

# ── runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates is needed for HTTPS calls to gnomonitoring and gno.land RPC.
# wget (busybox, part of the base image) backs the compose healthcheck on /health.
RUN apk add --no-cache ca-certificates \
	&& adduser -D -u 10001 katana

WORKDIR /app

# Data dir for the SQLite database and the gnokey keybase. Created with the right
# ownership so a fresh named volume mounted here is writable by the katana user.
RUN mkdir -p /app/data && chown -R katana:katana /app

COPY --from=build /out/katana /usr/local/bin/katana
COPY --from=gnokey /go/bin/gnokey /usr/local/bin/gnokey

USER katana

# katana is a long-running daemon; it loops internally until it receives a
# termination signal. No wrapper/sleep loop needed.
ENTRYPOINT ["katana"]
CMD ["--config", "/app/config.yaml"]
