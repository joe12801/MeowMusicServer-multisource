#!/bin/bash
# MeowMusicServer-multisource + 汽水音乐增强 一键部署/升级脚本
#
# 整合：官方最新镜像 + mp3api 外部音源 + 汽水登录态(cookie) + 汽水搜索代理(qishui-proxy)
# 放在 deploy/qishui/ 下使用；会从脚本同目录逐级向上找 .env 与 data/。
#
# 用法：
#   bash deploy/qishui/upgrade.sh
#   FORCE_PULL=1 bash deploy/qishui/upgrade.sh   # 强制重新拉镜像
#
set -e

# 脚本所在目录 -> 部署根目录（默认取上级 deploy 的父目录，可用 MEOW_HOME 覆盖）
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
MEOW_HOME="${MEOW_HOME:-$(cd "$SCRIPT_DIR/../.." && pwd)}"
ENV_FILE="${MEOW_HOME}/.env"

if [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
fi
KEY="${MUSIC_MP3API_KEY:-}"
if [ -z "$KEY" ]; then
  echo "警告: 未找到 MUSIC_MP3API_KEY（应配置在 $ENV_FILE），mp3api 外部音源将不可用"
fi

echo "[0/5] 准备目录、网络与配置文件"
mkdir -p "$MEOW_HOME"/data/files "$MEOW_HOME"/data/cache "$MEOW_HOME"/data/qishui "$MEOW_HOME"/qishui-proxy
[ -f "$MEOW_HOME"/data/devices.json ] || echo '{"devices":{}}' > "$MEOW_HOME"/data/devices.json
[ -f "$MEOW_HOME"/data/qishui/cookies.json ] || echo '{}' > "$MEOW_HOME"/data/qishui/cookies.json
chown 1000:1000 "$MEOW_HOME"/data/qishui/cookies.json 2>/dev/null || true
chmod 644 "$MEOW_HOME"/data/qishui/cookies.json
docker network inspect meowmusic-net >/dev/null 2>&1 || docker network create meowmusic-net

# 代理脚本必须存在，否则中止，避免升级后搜索变空
if [ ! -f "$MEOW_HOME"/qishui-proxy/proxy.cjs ]; then
  echo "错误: 缺少 $MEOW_HOME/qishui-proxy/proxy.cjs"
  echo "请把 deploy/qishui/proxy.cjs 复制到该路径后再运行。"
  exit 1
fi

echo "[1/5] 拉取最新镜像（官方源优先，失败回落国内加速器）"
if [ "${FORCE_PULL:-0}" = "1" ]; then
  docker pull joe1280/meowmusic:latest || \
    (docker pull docker.1ms.run/joe1280/meowmusic:latest && \
     docker tag docker.1ms.run/joe1280/meowmusic:latest joe1280/meowmusic:latest) || true
  docker pull guohuiyuan/go-music-api:latest || \
    (docker pull docker.1ms.run/guohuiyuan/go-music-api:latest && \
     docker tag docker.1ms.run/guohuiyuan/go-music-api:latest guohuiyuan/go-music-api:latest) || true
else
  echo "跳过拉取（如需强制拉取请设 FORCE_PULL=1）"
fi
echo "  主服务镜像: $(docker inspect joe1280/meowmusic:latest --format '{{.Created}} {{.Id}}' 2>/dev/null)"
echo "MEOW_IMAGE_DIGEST=$(docker inspect joe1280/meowmusic:latest --format '{{.Id}}' 2>/dev/null)" > "$MEOW_HOME"/.image_version

echo "[2/5] 重建 gomusic sidecar（汽水榜单/取链，加载登录 cookie）"
docker rm -f gomusic 2>/dev/null || true
docker run -d --name gomusic --restart unless-stopped \
  --network meowmusic-net -e TZ=Asia/Shanghai \
  -v "$MEOW_HOME"/data/qishui/cookies.json:/home/appuser/cookies.json \
  guohuiyuan/go-music-api:latest >/dev/null

echo "[3/5] 重建 qishui-proxy（汽水全文搜索：改走免签名公开搜索）"
docker rm -f qishui-proxy 2>/dev/null || true
docker run -d --name qishui-proxy --restart unless-stopped \
  --network meowmusic-net \
  -v "$MEOW_HOME"/qishui-proxy/proxy.cjs:/app/proxy.cjs:ro \
  --entrypoint node w3126197382/meting-api:latest /app/proxy.cjs >/dev/null

echo "[4/5] 重建 meow-music 主服务（最新代码 + 全部环境变量）"
docker rm -f meow-music 2>/dev/null || true
docker run -d --name meow-music --restart unless-stopped \
  --network meowmusic-net -p 2233:2233 \
  -e TZ=Asia/Shanghai -e PLAYLIST_SONGS_MAX=150 \
  -e MUSIC_MP3API_KEY="$KEY" \
  -e QISHUI_BASE=http://qishui-proxy:8080 \
  -v "$MEOW_HOME"/data/files:/app/files \
  -v "$MEOW_HOME"/data/cache:/app/cache \
  -v "$MEOW_HOME"/data/devices.json:/app/devices.json \
  joe1280/meowmusic:latest >/dev/null

echo "[5/5] 等待健康检查并验证"
sleep 20
echo "--- 容器状态 ---"
docker ps --filter name=meow-music --filter name=gomusic --filter name=qishui-proxy \
  --format '{{.Names}}\t{{.Status}}'
echo "--- 主服务 HTTP ---"
curl -s -o /dev/null -w 'GET / -> HTTP %{http_code}\n' http://127.0.0.1:2233/
echo "升级完成。"
