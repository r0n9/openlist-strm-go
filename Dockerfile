# 阶段 1：构建阶段
FROM golang:1.25-alpine AS builder

WORKDIR /build

# 安装基础依赖 (根证书和时区)
RUN apk add --no-cache ca-certificates tzdata

# 缓存依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制源码并编译
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o openlist-strm-go ./main.go

# 阶段 2：运行阶段
FROM alpine:latest

WORKDIR /app

# 从构建阶段复制证书和时区配置
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo/Asia/Shanghai /etc/localtime
RUN echo "Asia/Shanghai" > /etc/timezone

# 复制编译好的二进制文件
COPY --from=builder /build/openlist-strm-go .

# 创建默认的数据存放目录
RUN mkdir -p /app/data

# 默认环境变量
ENV STRM_SAVE_PATH=/app/data

# 执行程序
CMD ["./openlist-strm-go"]