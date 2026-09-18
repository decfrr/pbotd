.PHONY: build install test race vet check

build:
	CGO_ENABLED=0 go build -trimpath -o bin/pbotd ./cmd/pbotd

install:
	./scripts/install.sh

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: test race vet build
	test -z "$$(gofmt -l cmd internal tests)"
