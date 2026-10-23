# ai-decision-service/Dockerfile - Dockerfile for building the AI Decision Service

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
WORKDIR /workspace
ARG TARGETOS
ARG TARGETARCH

# Root module: your go.mod replaces it with ../, so the build needs it here.
COPY go.mod go.sum ./
COPY pkg/ pkg/
COPY api/ api/
COPY ai-decision-service/ ai-decision-service/

RUN cd ai-decision-service && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /usr/local/bin/ai-decision-service ./cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /usr/local/bin/ai-decision-service /usr/local/bin/ai-decision-service
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ai-decision-service"]
