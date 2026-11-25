#!/bin/sh
# warm-playlist.sh - 把歌单里的歌提前转码成缓存，播放时直接命中（秒开）。
#
# 背景：这台 NAS 的 CPU 编码一首 4 分钟的歌要 15~20 秒（见 deploy/README 的排障节），
# 未缓存的歌首次播放必须整首转码完才回包，设备端就是"点了半天没声音"。
# 提前把这批歌转完，之后播放只需 0.05 秒返回。
#
# 用法（在 NAS 宿主机上跑）：
#   ./warm-playlist.sh              # 转全部歌单（已缓存的最多 0.1 秒跳过）
#   ./warm-playlist.sh 20           # 只处理前 20 首，用于验证
#   nohup ./warm-playlist.sh > /tmp/warm.log 2>&1 &
#
# 注意：
#   - 走本机 127.0.0.1，不经过公网；命中缓存时不会产生任何转码。
#   - 串行执行，任意时刻只占一个 ffmpeg，不会把正在播放的歌挤掉。
#   - 幂等，可以重复跑；新增歌单条目后再跑一次即可补齐。
#   - 默认从 devices.json 取已绑定设备的 token（游客每日只有 20 首，会 429）；
#     也可用 MEOW_DEVICE_TOKEN=xxx 直接指定。

set -u

BASE="${MEOW_BASE:-http://127.0.0.1:2233}"
PLAYLIST_FILE="${PLAYLIST_FILE:-/vol1/docker/meowmusic/data/files/user_playlists.json}"
DEVICES_FILE="${DEVICES_FILE:-/vol1/docker/meowmusic/data/devices.json}"
LIMIT="${1:-0}"

# 身份：游客每日只有 GUEST_DAILY_SONGS 首额度（默认 20），预热几十首必然撞 429。
# 优先用环境变量传入的 token，否则从 devices.json 里取第一台已绑定设备的 token。
TOKEN="${MEOW_DEVICE_TOKEN:-}"
if [ -z "$TOKEN" ] && [ -f "$DEVICES_FILE" ]; then
    TOKEN=$(grep -o '"token"[[:space:]]*:[[:space:]]*"[^"]*"' "$DEVICES_FILE" \
        | head -1 | sed -E 's/.*"([^"]*)"$/\1/')
fi

if [ -n "$TOKEN" ]; then
    AUTH_HEADER="X-Device-Token: $TOKEN"
    echo "== 身份: 设备 token（不受游客额度限制）"
else
    AUTH_HEADER=""
    echo "== 警告: 没找到设备 token，将按游客身份请求，超过每日额度会返回 429" >&2
fi

if [ ! -f "$PLAYLIST_FILE" ]; then
    echo "找不到歌单文件: $PLAYLIST_FILE" >&2
    exit 1
fi

TAB=$(printf '\t')
TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT

# JSON 里每首歌是 "title" 在前、"artist" 在后，按行配对即可，无需完整 JSON 解析。
awk -F'"' '/"title":/{t=$4} /"artist":/{if (t != "") print $4 "\t" t}' "$PLAYLIST_FILE" \
    | sort -u > "$TMP"

TOTAL=$(wc -l < "$TMP")
[ "$LIMIT" -gt 0 ] 2>/dev/null && TOTAL=$((TOTAL < LIMIT ? TOTAL : LIMIT))

echo "== 待处理 $TOTAL 首（歌单去重后 $(wc -l < "$TMP") 首）"
echo "== 目标: $BASE"

i=0
warm=0
hit=0
fail=0
start=$(date +%s)

while IFS="$TAB" read -r artist title; do
    [ -z "${title:-}" ] && continue
    i=$((i + 1))
    if [ "$LIMIT" -gt 0 ] 2>/dev/null && [ "$i" -gt "$LIMIT" ]; then
        break
    fi

    t0=$(date +%s)
    if [ -n "$AUTH_HEADER" ]; then
        code=$(curl -s -o /dev/null -w '%{http_code}' -m 180 -H "$AUTH_HEADER" -G "$BASE/stream_live" \
            --data-urlencode "song=$title" --data-urlencode "singer=$artist")
    else
        code=$(curl -s -o /dev/null -w '%{http_code}' -m 180 -G "$BASE/stream_live" \
            --data-urlencode "song=$title" --data-urlencode "singer=$artist")
    fi
    t1=$(date +%s)
    cost=$((t1 - t0))

    if [ "$code" != "200" ]; then
        fail=$((fail + 1))
        echo "[$i/$TOTAL] FAIL $code  ${cost}s  $artist - $title"
    elif [ "$cost" -le 2 ]; then
        hit=$((hit + 1))
        echo "[$i/$TOTAL] 命中缓存  ${cost}s  $artist - $title"
    else
        warm=$((warm + 1))
        echo "[$i/$TOTAL] 新转码    ${cost}s  $artist - $title"
    fi
done < "$TMP"

end=$(date +%s)
echo "== 完成: 新转码 $warm 首, 已有缓存 $hit 首, 失败 $fail 首, 总耗时 $((end - start))s"
