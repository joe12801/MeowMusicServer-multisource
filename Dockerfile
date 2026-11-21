# syntax=docker/dockerfile:1

# ---------- 构建阶段 ----------
FROM golang:1.25-alpine AS builder

WORKDIR /src

# 国内网络优先走 goproxy.cn，失败再回落官方
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV CGO_ENABLED=0 \
    GOFLAGS=-mod=mod \
    GOPROXY=${GOPROXY}

# 先拷依赖清单，源码改动可复用依赖层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY theme ./theme
COPY frontend ./frontend

RUN go build -trimpath -ldflags="-s -w" -o /out/MeowMusicServer .

# ---------- 运行阶段 ----------
FROM alpine:3.20

# ffmpeg/ffprobe 是转码与时长探测的硬依赖，wget 供 healthcheck 用
RUN apk add --no-cache ffmpeg ca-certificates tzdata wget

WORKDIR /app

COPY --from=builder /out/MeowMusicServer /app/MeowMusicServer
COPY --from=builder /src/theme /app/theme
COPY --from=builder /src/frontend /app/frontend

# 首次启动的种子数据（运行时用 volume 覆盖，不会污染宿主数据）
COPY files /app/files

ENV PORT=2233 \
    TZ=Asia/Shanghai \
    PLAYLIST_SONGS_MAX=150

EXPOSE 2233

# 数据目录：歌单/用户/设备绑定/背景图在 files，音频缓存在 cache
VOLUME ["/app/files", "/app/cache"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT}/" || exit 1

CMD ["/app/MeowMusicServer"]
