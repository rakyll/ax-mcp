export GOTOOLCHAIN := auto

.PHONY: build demo demo-auto vet

build:
	go build -o bin/ ./cmd/...

vet:
	go vet ./...

demo: build
	./bin/ax-mcp-demo

demo-auto: build
	./bin/ax-mcp-demo -auto
