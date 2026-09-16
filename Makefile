BINARY := alert-agent

.PHONY: build test lint run release clean

build:
	go build -o bin/$(BINARY) ./cmd/alert-agent

test:
	go test ./...

lint:
	go vet ./...

run: build
	./bin/$(BINARY) -config config.yaml

DIST := dist
RELEASE := alert-agent-$(shell date +%Y%m%d)-linux-amd64

# 交付包：静态二进制 + 剧本 + 配置样例 + systemd 单元（目标机无需 Go）
# arm64 机器把 GOARCH 改掉重跑
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(DIST)/$(RELEASE)/bin/alert-agent ./cmd/alert-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(DIST)/$(RELEASE)/bin/mcp-stub ./cmd/mcp-stub
	cp -r skills/examples $(DIST)/$(RELEASE)/skills
	cp config.example.yaml deploy/alert-agent.service $(DIST)/$(RELEASE)/
	chmod -R a+rX $(DIST)/$(RELEASE)
	tar -C $(DIST) -czf $(DIST)/$(RELEASE).tar.gz $(RELEASE)
	@echo "交付包: $(DIST)/$(RELEASE).tar.gz"

clean:
	rm -rf bin $(DIST)
