.PHONY: all web build run test test-go test-web fuzz clean

all: build

web:
	cd web && npm ci --no-audit --no-fund && npm run typecheck && npm run build

build: web
	go build -o bin/oddsboard ./cmd/oddsboard

run:
	go run ./cmd/oddsboard

test: test-go test-web

test-go:
	go vet ./...
	go test -race ./...

test-web:
	cd web && npm run typecheck && npm test

# Explore the merge engine and parsers with Go's fuzzer (30s each).
fuzz:
	go test ./internal/market -run '^$$' -fuzz '^FuzzMergeInvariants$$' -fuzztime 30s
	go test ./internal/market -run '^$$' -fuzz '^FuzzParseEvent$$' -fuzztime 30s
	go test ./internal/market -run '^$$' -fuzz '^FuzzParsePrice$$' -fuzztime 30s

clean:
	rm -rf bin
