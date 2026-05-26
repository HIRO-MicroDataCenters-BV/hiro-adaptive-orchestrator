# scheduler-plugin/Dockerfile — HIRO Adaptive Orchestrator scheduler binary
#
# Build context must be the repo root (Docker needs go.mod, pkg/, etc.):
#   docker build -f scheduler-plugin/Dockerfile --build-arg K8S_VERSION=v1.35.0 -t <tag> .
#   (or use: make docker-build-scheduler from repo root)
#
# Build args:
#   K8S_VERSION  Kubernetes version the scheduler plugin is compiled against.
#                Must match the minor version of the target cluster.
#                Default: v1.35.0
#
# Image tag convention:
#   ghcr.io/.../hiro-scheduler:<project-version>-k8s<K8S_VERSION>
#   Example: hiro-scheduler:v0.1.0-k8sv1.35.0

# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.25 AS builder

WORKDIR /workspace

ARG K8S_VERSION=v1.35.0

# Copy the operator module files — scheduler-plugin depends on them via replace.
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY api/ api/
COPY internal/ internal/

# Copy the scheduler-plugin module (includes pin_k8s_version.sh).
COPY scheduler-plugin/ scheduler-plugin/

# Update go.mod staging versions for K8S_VERSION, then tidy deps.
RUN scheduler-plugin/pin_k8s_version.sh "$K8S_VERSION" \
 && cd scheduler-plugin && go mod tidy

# Build — CGO_ENABLED=0 produces a fully static binary for the distroless image.
RUN cd scheduler-plugin && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags="-s -w -X 'main.k8sVersion=${K8S_VERSION}'" \
      -o /usr/local/bin/hiro-scheduler \
      ./cmd/

# ── Final stage ───────────────────────────────────────────────────────────────
# gcr.io/distroless/static:nonroot — no shell, no package manager, uid 65532.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /usr/local/bin/hiro-scheduler /usr/local/bin/hiro-scheduler

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/hiro-scheduler"]
