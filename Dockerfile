FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version.version=${VERSION} \
      -X github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version.commit=${COMMIT} \
      -X github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version.date=${BUILD_DATE}" \
    -o /bin/oom-oracle ./cmd/oom-oracle

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/k8s-pod-oom-oracle"
LABEL org.opencontainers.image.description="Kubernetes system daemon and CLI for low-level process-aware OOM diagnostics using cgroups and eBPF tracing"
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /bin/oom-oracle /oom-oracle
ENTRYPOINT ["/oom-oracle"]
