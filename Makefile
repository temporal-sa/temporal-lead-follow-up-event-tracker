.PHONY: build test vet dev preview

build:
	go build -o bin/tracker ./cmd/tracker

test:
	go test -race ./...

vet:
	go vet ./...

dev:
	DEV_AUTH_EMAIL=developer@temporal.io go run ./cmd/tracker dev

preview:
	bash scripts/local-preview.sh
