.PHONY: run build test check

run:
	go run ./cmd/wiki-agent

build:
	go build -o bin/wiki-agent ./cmd/wiki-agent

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...
