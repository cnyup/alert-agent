BINARY := alert-agent

.PHONY: build test lint run release check-go clean

# xdag 依赖硬性要求 Go >= 1.27；版本不足时提前给出明确指引
check-go:
	@v=$$(go version 2>/dev/null | grep -oE 'go1\.[0-9]+' | head -1 | cut -d. -f2); \
	if [ -z "$$v" ] || [ "$$v" -lt 27 ]; then \
		echo "错误: 需要 Go >= 1.27（xdag 依赖硬性要求），当前: $$(go version 2>/dev/null || echo 未安装)"; \
		echo "安装: https://mirrors.aliyun.com/golang/ (Linux) 或 brew install go (Mac)"; \
		echo "或让当前 Go 自动拉取 toolchain: go env -w GOSUMDB=sum.golang.org GOPROXY=https://goproxy.cn,direct"; \
		exit 1; \
	fi

build: check-go
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
release: check-go
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(DIST)/$(RELEASE)/bin/alert-agent ./cmd/alert-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(DIST)/$(RELEASE)/bin/mcp-stub ./cmd/mcp-stub
	cp -r skills/examples $(DIST)/$(RELEASE)/skills
	cp config.example.yaml deploy/alert-agent.service $(DIST)/$(RELEASE)/
	chmod -R a+rX $(DIST)/$(RELEASE)
	tar -C $(DIST) -czf $(DIST)/$(RELEASE).tar.gz $(RELEASE)
	@echo "交付包: $(DIST)/$(RELEASE).tar.gz"

clean:
	rm -rf bin $(DIST)
