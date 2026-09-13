.PHONY: test build run fmt fmt-check vet ci
test:
	go test -race ./...
build:
	go build -o bin/relaytale ./cmd/relaytale
run:
	go run ./cmd/relaytale
fmt:
	gofmt -w .
vet:
	go vet ./...

fmt-check:
	sh scripts/check-format.sh
ci: fmt-check vet
	go build ./...
	REQUIRE_INTEGRATION_TESTS=1 go test -race -count=1 -timeout=8m ./...
