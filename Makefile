# Copyright 2026 Red Hat, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

SHELL              := /usr/bin/env bash -o pipefail

APP_NAME           := aro-pull-secret-operator
IMAGE_REPO         ?= quay.io/naeemarsalan/$(APP_NAME)
IMAGE_TAG          ?= dev
IMAGE              := $(IMAGE_REPO):$(IMAGE_TAG)
RUNTIME            ?= podman

GO                 ?= go
GOBIN              ?= $(shell $(GO) env GOPATH)/bin
GOOS               ?= $(shell $(GO) env GOOS)
GOARCH             ?= $(shell $(GO) env GOARCH)
LDFLAGS            := -s -w

NAMESPACE          ?= aro-pull-secret-operator

# Default target.
.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} \
	  /^[a-zA-Z0-9_.-]+:.*##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run gofmt against the code.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet against the code.
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum.
	$(GO) mod tidy

.PHONY: test
test: ## Run unit tests.
	$(GO) test -race -count=1 -coverprofile=cover.out ./...

.PHONY: verify
verify: fmt vet tidy ## Run all static checks. Fails CI if anything changed.
	@git diff --exit-code -- go.mod go.sum

##@ Build

.PHONY: build
build: ## Build the manager binary into ./bin/manager.
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	  $(GO) build -trimpath -ldflags='$(LDFLAGS)' -o ./bin/manager ./cmd/manager

.PHONY: image
image: ## Build the operator container image.
	$(RUNTIME) build -t $(IMAGE) -f Dockerfile .

.PHONY: image-push
image-push: ## Push the operator container image.
	$(RUNTIME) push $(IMAGE)

##@ Deploy

.PHONY: deploy
deploy: ## Apply manifests to the currently-targeted cluster.
	oc apply -f config/manager/namespace.yaml
	oc apply -f config/rbac/rbac.yaml
	oc apply -f config/manager/deployment.yaml

.PHONY: undeploy
undeploy: ## Remove the operator from the cluster. Leaves managed Secrets intact.
	-oc delete -f config/manager/deployment.yaml
	-oc delete -f config/rbac/rbac.yaml
	-oc delete -f config/manager/namespace.yaml

##@ Housekeeping

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf ./bin cover.out
