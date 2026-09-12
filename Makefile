.PHONY: test build run fmt vet
test:
	go test -race ./...
build:
	go build -o bin/relaytale ./cmd/relaytale
run:
	go run ./cmd/relaytale
fmt:
	gofmt -w cmd internal migrations
vet:
	go vet ./...
