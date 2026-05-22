# syntax=docker/dockerfile:1.6
#
# Multi-stage build using Red Hat UBI images. The final image is
# ubi9-micro which is significantly smaller than ubi-minimal and
# contains only what is needed to run a static Go binary.

FROM registry.access.redhat.com/ubi9/go-toolset:1.23 AS builder
WORKDIR /workspace

# Copy module files first to maximise layer caching.
COPY go.mod go.sum* ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# CGO disabled so the binary is fully static and runs on ubi-micro.
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS="-trimpath" \
    go build -ldflags="-s -w" -o /workspace/manager ./cmd/manager

FROM registry.access.redhat.com/ubi9/ubi-micro:latest
WORKDIR /
COPY --from=builder /workspace/manager /usr/bin/aro-pull-secret-operator

USER 65532:65532

LABEL name="aro-pull-secret-operator" \
      summary="Reconciles cluster pull secret from multiple additional pull secrets" \
      io.k8s.display-name="ARO Pull Secret Operator" \
      io.openshift.tags="openshift,aro,pull-secret"

ENTRYPOINT ["/usr/bin/aro-pull-secret-operator"]
