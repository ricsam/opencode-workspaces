SHELL := /usr/bin/env bash
GO ?= go
HELM ?= helm
MINT ?= mint

.PHONY: fmt test build lint helm-verify docs-verify opencode-verify test-patches verify images

fmt:
	$(GO) fmt ./...

test:
	$(GO) test ./...

build:
	CGO_ENABLED=0 $(GO) build -o bin/opencode-workspaces ./cmd/server

lint:
	$(GO) vet ./...

helm-verify:
	$(HELM) lint charts/opencode-workspaces
	$(HELM) template verify charts/opencode-workspaces --namespace opencode-workspaces >/tmp/opencode-workspaces.yaml
	kubectl apply --dry-run=client -f /tmp/opencode-workspaces.yaml >/dev/null

docs-verify:
	cd docs && $(MINT) validate
	cd docs && $(MINT) broken-links --check-anchors --check-redirects

opencode-verify:
	./scripts/with-patched-opencode.sh bash -ec 'bun install --frozen-lockfile; bun run --cwd packages/app typecheck; bun run --cwd packages/app build'

test-patches:
	./scripts/with-patched-opencode.sh bash -ec 'git diff --check; test -n "$$(git status --short)"'

verify: fmt lint test helm-verify docs-verify test-patches opencode-verify

images:
	docker build -f build/Dockerfile.control-plane -t opencode-workspaces-control-plane:dev .
	docker build -f build/Dockerfile.workspace -t opencode-workspaces-workspace:dev .
