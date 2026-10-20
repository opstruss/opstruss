.PHONY: build test criteria lint check snapshot

build:
	go build -o bin/opstruss .

test:
	go test -race ./...
	go test -race -tags failpoints -run 'TestScripts/crash' .

criteria:
	OPSTRUSS_CRITERIA=1 go test -tags failpoints -timeout 30m -run TestCriteria .

lint:
	golangci-lint run
	go mod tidy -diff
	actionlint
	goreleaser check

check: lint test build

snapshot:
	goreleaser release --snapshot --clean
