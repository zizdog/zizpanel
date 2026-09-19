# 未在 fnOS 验证：这是草图，没有在飞牛设备上构建或运行过。
# 媒体目录只读挂载；数据库与封面写在单独的可写卷里。
# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/zizdog/govideo/internal/api.Version=${VERSION}" \
      -o /out/govideo ./cmd/server

FROM debian:bookworm-slim
# 未验证：基础镜像里装的 ffmpeg 版本没有在 fnOS 上跑过。
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/govideo /usr/local/bin/govideo
# 以非 root 运行；UID/GID 需要与媒体目录属主一致，现场确认后再定。
RUN useradd -r -u 1000 -m -d /var/lib/govideo govideo \
 && mkdir -p /var/lib/govideo/covers && chown -R 1000:1000 /var/lib/govideo
USER 1000:1000
ENV GV_LISTEN=0.0.0.0:7766 \
    GV_DATA_DIR=/var/lib/govideo \
    GV_DATABASE_PATH=/var/lib/govideo/govideo.db \
    GV_FFMPEG_BIN=/usr/bin/ffmpeg \
    GV_FFPROBE_BIN=/usr/bin/ffprobe
EXPOSE 7766
ENTRYPOINT ["/usr/local/bin/govideo"]
