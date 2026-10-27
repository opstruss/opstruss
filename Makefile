.PHONY: build test criteria load lint check snapshot docs docs-serve docs-gen

build:
	go build -o bin/opstruss .

test:
	go test -race ./...
	go test -race -tags failpoints -run 'TestScripts/crash' .

criteria:
	OPSTRUSS_CRITERIA=1 go test -tags failpoints -timeout 30m -run 'TestCriteria|TestUseCases|TestLoad' .

# load runs the load criteria for ten minutes, as the daily load workflow does.
load:
	OPSTRUSS_CRITERIA=1 OPSTRUSS_LOAD_SECONDS=600 go test -tags failpoints -timeout 60m -run 'TestLoad' .

lint:
	golangci-lint run
	go mod tidy -diff
	actionlint
	goreleaser check

check: lint test build

snapshot:
	goreleaser release --snapshot --clean

# docs builds the documentation as /dev/ into docs/.vitepress/dist;
# DOCS_BASE=/v0.1/ builds it as a release.
docs:
	cd docs && npm ci && npx vitepress build

docs-serve:
	cd docs && npm ci && npx vitepress dev

# docs-gen writes the reference pages of the schemas and the commands.
docs-gen:
	go run ./internal/docgen docs/reference
