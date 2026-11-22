# 设备遥控协议（Web → 服务器 → ESP32）

音箱端不需要开端口、不需要公网 IP、不需要 MQTT，只要**主动轮询**服务器即可。
服务器把指令放在内存队列里，设备取走后执行，再回报结果。

```
手机/Web  ──POST /api/device/command──▶  服务器内存队列（每设备最多 50 条，5 分钟过期）
ESP32     ──GET  /api/esp32/poll──────▶  有指令立即返回，没有就挂 20s（长轮询）
ESP32     ──POST /api/esp32/ack───────▶  回报执行结果（只用于面板展示）
```

## 1. Web 端下发指令

```
POST /api/device/command
Authorization: Bearer <登录 token>      或 Cookie: session_token=...
Content-Type: application/json

{"mac": "AA:BB:CC:DD:EE:FF", "action": "volume", "params": {"value": 42}}
```

响应：

```json
{"success": true, "id": "d75f251131a7b1db", "queued": 1}
```

约束：

- 只能操作**自己绑定的设备**（服务端会核对 `devices.json` 里的 username），否则 403。
- `action` 必须在白名单内，否则 400：

  `play` `pause` `resume` `stop` `next` `prev` `volume` `seek`
  `play_song` `play_playlist` `reload_playlist` `set_loop` `set_shuffle` `reboot`

- 指令 5 分钟未被取走自动过期；同一设备最多积压 50 条，超出丢最旧的。

## 2. 设备端轮询（固件需要新增的就是这一段）

```
GET /api/esp32/poll?mac=AA:BB:CC:DD:EE:FF&wait=20
X-Device-Token: <绑定后拿到的设备 token>
```

- `wait` 是长轮询秒数，0~55，默认 20。**为空立即返回，有指令也立即返回。**
- `mac` 可省略（默认取 token 对应的设备）；传了但不匹配返回 403。

响应：

```json
{
  "success": true,
  "mac": "AA:BB:CC:DD:EE:FF",
  "server_time": 1795293559,
  "commands": [
    {
      "id": "3c5d9b7e79fb806b",
      "action": "volume",
      "params": {"value": 42},
      "created_at": "2026-11-22T04:39:19+08:00",
      "expires_at": "2026-11-22T04:44:19+08:00"
    }
  ]
}
```

`commands` 为空数组表示这轮没有指令，直接再发一次轮询即可。

建议固件循环：**poll(wait=20) → 逐条执行 → ack → 立刻再 poll**。
网络断开时按 2s/5s/10s 退避重试。

## 3. 回报结果

```
POST /api/esp32/ack
X-Device-Token: <设备 token>
Content-Type: application/json

{"id": "3c5d9b7e79fb806b", "ok": true}
```

失败时带 `"error": "原因"`，会显示在 `/remote` 页面的「最近回报」里。

## 4. Web 控制页

浏览器打开 **`/remote`**：选设备 → 上一首/暂停/继续/下一首/停止、
音量滑杆、指定歌曲播放、重载歌单。页面每 5 秒刷新一次设备在线状态与待取指令数。

`/remote` 与 `/status` 都需要先登录，未登录会返回 401 并显示登录框（登录接口会写
HttpOnly 的 `session_token` Cookie，登录后自动回到原页面）。服务器在 `.env` 里设了
`ADMIN_PAGE_TOKEN=xxx` 时，也可以用 `/status?key=xxx` 应急进入。

## 5. 安全说明

- 设备侧只认 `X-Device-Token`，且 token 与 MAC 必须对应；拿不到别人的指令。
- 指令只在内存里，进程重启即清空（不会误放历史指令）。
- 设备 token 等同于播放凭证，泄露了就在网页「设备绑定」页解绑重绑。

## 6. ESP32 固件（可直接编译）

仓库里带了一份完整示例，改 4 个常量就能烧：

```
firmware/esp32_remote_poll/esp32_remote_poll.ino
```

- 依赖 `ArduinoJson`（6.21.x / 7.x 都已兼容，无需改代码）；
- 开机先 `GET /api/esp32/verify` 自检 token，串口会打印绑定的账号；
- 主循环 `poll(wait=20) → 逐条执行 → ack → 立刻再 poll`；
- 动作分发集中在 `executeCommand()`，把你自己的播放函数填进 `doPlay()` /
  `doVolume()` 等空函数里即可；
- WiFi 掉线自动重连，token 错（401/403）会退避 10 秒而不是死循环刷日志。

## 7. 没有硬件时怎么验证

用 `firmware/poll_test.sh` 把电脑当设备，配合 `/remote` 页面点按钮：

```bash
# 设备 token 在 devices.json 里
sudo grep -o '"token":"[^"]*"' /vol1/docker/meowmusic/data/devices.json

SERVER=http://<SERVER_IP>:2233 \
TOKEN=<token> MAC=<设备MAC> \
bash firmware/poll_test.sh
```

页面点「下一首」，脚本应立即打印 JSON 并自动 ack，面板上的「最近回报」也会更新。

注意：`wait=20` 时连接会静默 20 秒，`timeout_ms` 必须大于它，否则会被自己判超时。
