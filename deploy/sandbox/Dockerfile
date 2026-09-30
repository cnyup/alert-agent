# 按 https://sso.example.com/index/devops-cli/image-example 官方示例：
# token 不进镜像层，运行时经 OPS_CLI_TOKEN 注入
FROM debian:bookworm-slim

ARG CLI_VERSION=1.0.15
ARG CLI_PLATFORM=linux-arm64

ENV PATH="/root/.ops-cli/bin:${PATH}"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /root/.ops-cli/bin; \
    curl -fsSL -o /root/.ops-cli/bin/ops-cli \
      "https://assets.example.com/assets/ops-cli/${CLI_VERSION}/${CLI_PLATFORM}/ops-cli"; \
    chmod +x /root/.ops-cli/bin/ops-cli; \
    ops-cli --version

# 沙箱要求容器常驻（exec 复用同一容器）；CLI 自检挪到 RUN 阶段
CMD ["sleep", "infinity"]
