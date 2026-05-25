.PHONY: build test race lint bench fmt tidy run

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

bench:
	go test -bench . ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

run:
	go run ./cmd/mycypher
