.PHONY: test build run fmt vet
test:
	go test -race ./...
build:
	go build -o bin/gateway ./cmd/gateway
run:
	go run ./cmd/gateway
fmt:
	gofmt -w cmd internal migrations
vet:
	go vet ./...
