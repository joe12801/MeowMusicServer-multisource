#!/usr/bin/env bash
# 设备端模拟器：没有硬件时用它验证「下发指令 → 设备取走 → 回报」全链路。
#
# 用法：
#   SERVER=http://<SERVER_IP>:2233 TOKEN=<设备token> MAC=AA:BB:CC:DD:EE:FF ./poll_test.sh
#
# 另开一个终端打开 http://服务器:2233/remote 点按钮，这里应立即打印收到的指令。

set -u

SERVER="${SERVER:-http://<SERVER_IP>:2233}"
TOKEN="${TOKEN:-}"
MAC="${MAC:-}"
WAIT="${WAIT:-20}"

if [ -z "$TOKEN" ]; then
  echo "缺少 TOKEN。先看看 devices.json：" >&2
  echo "  sudo grep -o '\"token\":\"[^\"]*\"' /vol1/docker/meowmusic/data/devices.json" >&2
  exit 1
fi

echo "== 校验 token =="
curl -sS -H "X-Device-Token: $TOKEN" "$SERVER/api/esp32/verify"; echo

echo "== 开始长轮询（Ctrl+C 退出）=="
while true; do
  resp=$(curl -sS -m $((WAIT + 20)) -H "X-Device-Token: $TOKEN" \
         "$SERVER/api/esp32/poll?wait=$WAIT${MAC:+&mac=$MAC}") || {
    echo "轮询失败，2 秒后重试"; sleep 2; continue;
  }

  # 极简解析：命令里没有嵌套引号，够用了
  if echo "$resp" | grep -q '"commands":\[\]'; then
    continue
  fi

  echo "$resp"
  echo "$resp" | tr ',' '\n' | grep -o '"id":"[^"]*"' | cut -d'"' -f4 | while read -r id; do
    [ -z "$id" ] && continue
    echo "  ack $id"
    curl -sS -H "X-Device-Token: $TOKEN" -H 'Content-Type: application/json' \
         -d "{\"id\":\"$id\",\"ok\":true}" "$SERVER/api/esp32/ack" >/dev/null
  done
done
