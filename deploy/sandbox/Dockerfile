# 按 https://yw-sso.ttyuyin.com/index/devops-cli/image-example 官方示例：
# token 不进镜像层，运行时经 TT_YW_AUTHORIZATION 注入
FROM debian:bookworm-slim

ARG TT_DEVOPS_VERSION=1.0.15
ARG TT_DEVOPS_PLATFORM=linux-arm64

ENV PATH="/root/.tt-devops-cli/bin:${PATH}"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /root/.tt-devops-cli/bin; \
    curl -fsSL -o /root/.tt-devops-cli/bin/tt-devops-cli \
      "https://skill.ttyuyin.com/assets/tt-devops-cli/${TT_DEVOPS_VERSION}/${TT_DEVOPS_PLATFORM}/tt-devops-cli"; \
    chmod +x /root/.tt-devops-cli/bin/tt-devops-cli; \
    tt-devops-cli --version

# 沙箱要求容器常驻（exec 复用同一容器）；CLI 自检挪到 RUN 阶段
CMD ["sleep", "infinity"]
