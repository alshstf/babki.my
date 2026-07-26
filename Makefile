.PHONY: test lint tidy ui ui-dev build test-all gen smoke

VERSION ?= dev

test:
	go test ./...

gen:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml
	cd web && npm run gen:api

lint:
	golangci-lint run

tidy:
	go mod tidy

ui:
	cd web && npm ci && npm run build

ui-dev:
	cd web && npm run dev

build: ui
	go build -tags embedui \
		-ldflags "-X babki.my/babki/internal/platform/version.Version=$(VERSION)" \
		-o babki ./cmd/babki

test-all: test
	go test -tags embedui ./web/

smoke:
	./scripts/smoke-api.sh http://localhost:8080
