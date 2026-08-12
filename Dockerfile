FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /bin/k8s-pod-oom-oracle ./cmd/k8s-pod-oom-oracle

FROM gcr.io/distroless/static-debian12:latest
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/k8s-pod-oom-oracle"
LABEL org.opencontainers.image.description="Kubernetes system daemon and CLI for low-level process-aware OOM diagnostics using cgroups and eBPF tracing"
COPY --from=build /bin/k8s-pod-oom-oracle /k8s-pod-oom-oracle
ENTRYPOINT ["/k8s-pod-oom-oracle"]
