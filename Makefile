.PHONY: test lint tidy ui ui-dev

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy

ui:
	cd web && npm ci && npm run build

ui-dev:
	cd web && npm run dev
