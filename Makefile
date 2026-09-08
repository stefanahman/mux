.PHONY: test lint

test:
	go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
