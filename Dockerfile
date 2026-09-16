# K8s 面向的生产镜像：多阶段构建，单静态二进制（CGO_ENABLED=0 + modernc sqlite 纯 Go）。
# skills/ 打进镜像作为兜底；生产用 ConfigMap 挂载覆盖（见 deploy/k8s/）。
FROM golang:1.27 AS build
WORKDIR /src
# 国内网络环境走模块代理（公司内有私服可 build-arg 覆盖：--build-arg GOPROXY=https://your-proxy）
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/alert-agent ./cmd/alert-agent

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 aa
WORKDIR /app
# 匿名卷/PVC 初始属主沿用镜像目录状态：预建并赋权，避免非 root 打不开 SQLite
RUN mkdir -p /app/data /app/mcp && chown -R aa:aa /app/data /app/mcp
COPY --from=build /out/alert-agent /app/bin/alert-agent
COPY skills/examples/ /app/skills/
COPY config.example.yaml /app/config.yaml
# 构建上下文可能携带 0600 权限（mutagen 同步保留本地模式），统一放开只读
RUN chmod -R a+rX /app/config.yaml /app/skills
USER aa
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["/app/bin/alert-agent"]
CMD ["-config", "/app/config.yaml"]
