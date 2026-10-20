.PHONY: build test lint check snapshot

build:
	go build -o bin/opstruss .

test:
	go test -race ./...
	go test -race -tags failpoints -run 'TestScripts/crash' .

lint:
	golangci-lint run
	go mod tidy -diff
	actionlint
	goreleaser check

check: lint test build

snapshot:
	goreleaser release --snapshot --clean
