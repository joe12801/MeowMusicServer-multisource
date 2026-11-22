# 汽水音乐增强包（qishui enhance）

给主项目补上「汽水音乐」音源的完整可用链路。主服务本身已内置汽水筛选条与
`QISHUI_BASE` 接入点，但有两个坑需要这个增强包解决：

1. **sidecar**：汽水取链 / 榜单 / 扫码登录依赖 `guohuiyuan/go-music-api`（容器内 `gomusic`）。
2. **搜索签名**：`api.qishui.com/luna/pc/search/track` 已要求客户端原生签名头
   （`x-helios` / `x-medusa`），未签名请求返回 `HTTP 200 + 空 body`，`soda.Search`
   拿到空结果 —— 表现为「登录成功但搜不到任何歌」。本包用一个轻量代理把搜索改走
   免签名的公开接口，其余路径原样转发。

---

## 组成

| 文件 | 作用 |
|---|---|
| `proxy.cjs` | 搜索代理：`/api/v1/music/search` 改走公开搜索，其余透传给 `gomusic` |
| `upgrade.sh` | 一键部署/升级：拉最新镜像 + 重建 `gomusic` / `qishui-proxy` / `meow-music` |
| `.env.example` | 增强包配置项说明（密钥请放在部署根目录 `.env`，勿提交） |

> 代理脚本用的是 `.cjs` 扩展名：meting-api 镜像里 `/app/package.json` 带
> `"type": "module"`，用 `.js` 会报 `require is not defined`。

---

## 部署

前提：目标机器有 Docker（20.10+），出口 IP 在国内（境外 IP 会被汽水风控）。
下面假设部署根目录为 `/opt/meowmusic`（可用 `MEOW_HOME` 覆盖）。

```bash
MEOW_HOME=/opt/meowmusic
mkdir -p $MEOW_HOME/data/qishui $MEOW_HOME/qishui-proxy

# 1) 放代理脚本
cp deploy/qishui/proxy.cjs $MEOW_HOME/qishui-proxy/proxy.cjs

# 2) 配置密钥：放到部署根目录 .env（权限 600，勿提交）
#    MUSIC_MP3API_KEY=你的key
touch $MEOW_HOME/.env && chmod 600 $MEOW_HOME/.env

# 3) 准备汽水 cookie 占位（登录态在下一步扫码后写入）
echo '{}' > $MEOW_HOME/data/qishui/cookies.json
chown 1000:1000 $MEOW_HOME/data/qishui/cookies.json

# 4) 一键部署
bash deploy/qishui/upgrade.sh
```

升级到上游最新代码（默认复用本地已有镜像，加 `FORCE_PULL=1` 才重新拉）：

```bash
FORCE_PULL=1 bash deploy/qishui/upgrade.sh
```

---

## 汽水扫码登录（解锁全库搜索与完整播放）

未登录时汽水源只有榜单快照与 60 秒试听；登录后才能搜全库、完整播放。
登录走 `myedunote/meting-api` 的带签名扫码链路（裸 curl 会 hit 风控 `1049`）：

```bash
# 启动 meting-api（仅需登录时用，登录完可停）
docker run -d --name openmusic-meting-api --restart unless-stopped \
  -p 3000:3000 -v /opt/meting-api/data:/app/data \
  w3126197382/meting-api:latest
# 默认后台 admin / admin123，登录后取 X-Auth-Token
```

流程（会话 TTL 约 3 分钟，务必连贯操作）：

1. `POST /admin/qr/create` `{"platform":"qishui"}` 生成二维码，取 `data.qrimg`（base64 PNG）与 `data.token`。
2. 用**目标汽水账号**的汽水 App 扫码并在手机确认。
3. 若返回 `error_code 2046` / `status=second_verify`：先扫码触发后再打开
   `http://<host>:3000/admin/qr/qishui/security/security_host.html?key=<token>&bridgeRoot=/admin/qr/qishui/verify`，
   选短信验证、填验证码提交（号码需与账号绑定一致）。
4. `POST /admin/qr/check` `{"platform":"qishui","token":"<token>"}` 返回
   `status=confirmed` / `loggedIn=true` 后取 `data.cookie`。
5. 把 cookie 写入 `$MEOW_HOME/data/qishui/cookies.json`：
   `{"soda": "<上一步的 cookie 字符串>"}`，`chown 1000:1000`，然后
   `docker restart gomusic` 热加载。

注意：抖音 / creator.douyin.com 域的 cookie 对汽水无效，且会招致风控，不要用。

---

## 验证

```bash
curl -s 'http://127.0.0.1:2233/api/search?query=光辉岁月&source=qishui' | head -c 300
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' \
  'http://127.0.0.1:2233/stream_live?song=光辉岁月&singer=Beyond'   # 期望 200 audio/mpeg
```

代理本身健康：`docker logs qishui-proxy` 应只有 `listening on 0.0.0.0:8080`。

---

## 排障

| 现象 | 处理 |
|---|---|
| 汽水搜索为空但榜单正常 | 检查 `QISHUI_BASE` 是否指向 `http://qishui-proxy:8080`；看 `docker logs qishui-proxy` |
| `require is not defined` | 代理脚本必须用 `.cjs` 扩展名 |
| 扫码 hit `1049 应用版本过低` | 说明用了裸 curl；必须走 meting-api 的带签名扫码链路 |
| 榜单突然变空 | cookie 域不对或被风控；回滚 `cookies.json` 为 `{}` 并 `docker restart gomusic` |
| 拉镜像超时 | 升级脚本已内建国内加速器回落；或先配 Docker 代理 |
