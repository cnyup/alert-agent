BINARY := alert-agent

.PHONY: build test lint run clean

build:
	go build -o bin/$(BINARY) ./cmd/alert-agent

test:
	go test ./...

lint:
	go vet ./...

run: build
	./bin/$(BINARY) -config config.yaml

clean:
	rm -rf bin
