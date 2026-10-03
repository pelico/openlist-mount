# ---- Build stage ----
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Go 模块缓存层 (项目无依赖, 但保留模板)
COPY go.mod ./
RUN go mod download || true

# 源码
COPY *.go ./
COPY web/ ./web/

# go:embed 需要 web 目录在编译时存在
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/openlist-mount .

# ---- Runtime stage ----
FROM alpine:3.20

# rclone 必需, FUSE 依赖
RUN apk add --no-cache rclone ca-certificates tini

# openlist-mount 本身是 FUSE 守护进程, 需要 fuse
RUN apk add --no-cache fuse3 || apk add --no-cache fuse

COPY --from=builder /out/openlist-mount /usr/local/bin/openlist-mount

# 数据目录
VOLUME ["/var/lib/openlist-mount"]

EXPOSE 7777

# 默认启动: addr + data 都可用环境变量覆盖
ENV OLM_ADDR=:7777
ENV OLM_DATA=/var/lib/openlist-mount
ENV OLM_RCLONE=rclone

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["sh", "-c", "exec openlist-mount -addr $OLM_ADDR -data $OLM_DATA -rclone $OLM_RCLONE"]
