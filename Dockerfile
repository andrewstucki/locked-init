FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache dependencies.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both binaries statically (no CGO) for the target platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /bin/locked-init ./cmd/locked-init
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /bin/webhook ./cmd/webhook

# --- CLI wrapper image (used by initContainers) ---
FROM gcr.io/distroless/static AS locked-init

COPY --from=builder /bin/locked-init /bin/locked-init

ENTRYPOINT ["/bin/locked-init"]

# --- Webhook server image ---
FROM gcr.io/distroless/static AS webhook

COPY --from=builder /bin/webhook /bin/webhook

ENTRYPOINT ["/bin/webhook"]
