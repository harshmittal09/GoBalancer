# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 – Builder
# Uses the official Go image to compile a statically linked binary.
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

# Install git (needed by go mod download for VCS stamping) and CA certs.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Copy the module manifest first so Docker can cache the dependency layer.
COPY go.mod ./

# Download dependencies (none for stdlib-only project, but keeps structure valid).
RUN go mod download

# Copy the full source tree.
COPY . .

# Build a statically linked binary. CGO is disabled for a scratch-compatible image.
# -trimpath removes local file system paths from the binary for reproducibility.
# -ldflags "-s -w" strips debug symbols to reduce binary size.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /tcp-proxy \
      ./main.go

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 – Runtime
# Uses a minimal scratch image; only the static binary and CA certs are copied.
# ─────────────────────────────────────────────────────────────────────────────
FROM scratch

# Copy timezone data and CA certificates from the builder stage.
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the compiled binary.
COPY --from=builder /tcp-proxy /tcp-proxy

# The proxy listens on 8080 by default.
EXPOSE 8080

ENTRYPOINT ["/tcp-proxy"]
