# 多阶段构建：单静态二进制（CGO_ENABLED=0 + modernc sqlite 纯 Go）
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/alert-agent ./cmd/alert-agent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/mcp-stub ./cmd/mcp-stub

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 aa
WORKDIR /app
COPY --from=build /out/ /app/bin/
COPY --from=build /src/skills/examples/ /app/skills/
COPY --from=build /src/config.example.yaml /app/config.yaml
USER aa
EXPOSE 8080
VOLUME ["/app/data"]
ENTRYPOINT ["/app/bin/alert-agent"]
CMD ["-config", "/app/config.yaml"]
