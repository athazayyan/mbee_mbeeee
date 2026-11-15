.PHONY: check migrate

check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test ./...
	go build ./...

migrate:
	go run . migrate
