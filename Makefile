.PHONY: build test vet dev preview

build:
	go build -o bin/tracker ./cmd/tracker

test:
	go test -race ./...

vet:
	go vet ./...

# make dev expects DynamoDB Local, for example:
# docker run --rm -p 8000:8000 amazon/dynamodb-local:2.5.4 -jar DynamoDBLocal.jar -sharedDb -inMemory
DYNAMODB_ENDPOINT ?= http://localhost:8000

dev:
	DEV_AUTH_EMAIL=developer@temporal.io EVENT_LEADS_TABLE=event-leads-local DYNAMODB_ENDPOINT=$(DYNAMODB_ENDPOINT) \
		AWS_ACCESS_KEY_ID=local AWS_SECRET_ACCESS_KEY=local AWS_REGION=us-west-1 go run ./cmd/tracker dev

preview:
	bash scripts/local-preview.sh
