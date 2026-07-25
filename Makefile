.PHONY: test lint tidy

test:
	go test ./...

lint:
	golangci-lint run

tidy:
	go mod tidy
