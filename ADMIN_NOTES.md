# 本轮改动与运维说明（2026-11-22）

在原有安全加固（文件泄露、路径穿越、SSRF、设备同步鉴权、限流、原子写）
之上，这一轮补了 4 个功能、修了 2 个真实故障。

## 一、新增功能

| 功能 | 入口 | 说明 |
|---|---|---|
| 运行状态面板 | `GET /status` | 各音源成功率/平均耗时/最后错误、缓存占用、转码并发、磁盘剩余，10 秒自动刷新 |
| 音源健康接口 | `GET /api/sources` | 面板用的 JSON，也可以自己接监控 |
| 歌词 | `GET /api/lyric?song=&singer=&trans=1` | LRCLIB（带时间轴）→ 网易云兜底；`trans=1` 优先网易云以拿到 tlyric 翻译；结果落 `files/cache/lyrics/*.json`（原文 + 翻译） |
| 歌词页 | `GET /lyrics?song=&singer=&audio=` | 全屏滚动歌词，双语显示，带 `audio` 参数时会随播放高亮，点歌词可跳转 |
| 播放器歌词面板 | 播放器右侧 🎤 | 切歌自动加载、逐行高亮并居中、点行跳转、翻译小字显示、右上角 × 关闭，开合状态记在本地 |
| 播放器内嵌歌词 | 底部播放器最后一行 | 显示当前唱到的那句 + 翻译，随播放滚动；点击这行打开完整歌词面板；前奏显示「♪ 前奏…」，无词时显示「暂无歌词 · 点这里去搜索」 |
| 设备遥控 | `GET /remote` + `/api/device/command` | 网页给音箱切歌/调音量/指定歌曲；协议见 `DEVICE_CONTROL.md` |
| 设备固件示例 | `firmware/esp32_remote_poll/` | 可编译的 ESP32 轮询固件 + `poll_test.sh` 无硬件模拟器 |
| 运维页面鉴权 | `/status` `/remote` `/tools` | 未登录返回 401 + 登录框；`.env` 设 `ADMIN_PAGE_TOKEN` 后可用 `?key=口令` 应急进入 |
| 歌单工具页 | `GET /tools` | 列出自己的歌单、一键导出 m3u/txt、粘贴网易云链接导入 |
| 歌单导出 | `GET /api/user/playlists/export?id=&format=m3u\|txt\|json` | m3u 里每首指向本机 `/stream_live`，VLC/foobar 直接能播 |
| 网易云歌单导入 | `POST /api/user/playlists/import-netease` | 只取歌名/歌手/时长，播放仍走多源回退，不受版权限制；重复曲目自动跳过 |
| 每日转码额度 | 无需操作 | 按「登录用户 / 匿名 IP」统计每天转码首数，超了直接 429 不启动 ffmpeg |
| 时长回填 | 无需操作 | 转码/命中缓存后 ffprobe 量真实时长，写进同目录 `meta.json`，搜索列表自动补上 |
| 音源自动降级 | 无需操作 | 某源连续失败 5 次自动冷却 5 分钟，期间点歌直接跳过，不再每次白等超时 |
| 缓存自动回收 | 无需操作 | 默认 45 天没被用到就删；总量超 20GB 从最旧开始删到 90% |
| 播放历史 | 首页「最近」标签 | 记录每首歌播放次数与最后播放时间；「最近播放」「最常听」两个视图、全部播放、一键插队 |
| 待播队列 | 播放器 `☰` | 真正的播放队列：插到下一首、上下移、删除、打乱、存成歌单，刷新不丢 |
| 睡眠定时 | 播放器 `🌙` | 30/60/90 分钟或「本首播完停」，到时暂停播放 |
| PWA | 手机浏览器「添加到主屏幕」 | 有 manifest + 图标 + Service Worker，装到主屏后全屏无地址栏 |
| 数据自动备份 | 无需操作 | 每天 03:00 打包 users/sessions/歌单/设备到 `files/backups/`，留 7 份；`/status` 可手动触发 |
| 夜间预热 | 无需操作 | 每天 04:00 把歌单里没缓存的歌提前转码，第二天首播不用等 ffmpeg |

搜索接口现在会顺带返回 `lyric_url`，指向本机 `/api/lyric?...&format=lrc`，
前端可以直接拿来当 `<audio>` 的配套歌词地址。

### 歌词这条线做了什么

1. **双语**：`?trans=1` 时优先网易云（只有它给 `tlyric`），拿到翻译后按行贴到原文上；
   网易云没有就回落 LRCLIB（只有原文，至少有时间轴）。中文歌的 `tlyric` 通常是空的，
   所以「中文歌没有翻译行」是正常的，不是坏了。
2. **对齐**：翻译与原文行数相同时按顺序一一对应（网易云翻译版里没有「作词/作曲」那几行，
   按时间硬匹配会整首串位）；行数不同才退回 0.5 秒容差的时间匹配。
3. **过滤制作信息**：「作词 : 某某」「作曲 : 某某」这类行不再算歌词，既不占时间轴也不显示。
4. **防张冠李戴**：网易云搜不到时不再拿模糊结果顶替 —— 匹配分低于 30 直接判「无歌词」。
   之前搜「不存在的歌名zzz」会返回一首完全无关的歌的歌词，现在返回 404 语义的 JSON。
5. **缓存升级**：`files/cache/lyrics/<歌手-歌名>.json` 存原文+翻译+来源+是否试过翻译；
   老版本的同名 `.lrc` 仍能读，命中后会升级成 JSON 并删掉旧文件。
6. **前端**：切歌先清空面板（原来上一首歌词会残留继续高亮）；面板只在容器内滚动，
   不再带着整页跳；无歌词时给「去歌词页手动搜索」入口；搜索结果每行多了「歌词」直达链接。

## 二、修复的两个故障

**1. 网易云搜索整条链路失效（静默）**

`/api/search/get` 与 `/api/search/get/web` 两条接口在 2026-11 都变了：
前者返回完全不相干的结果（搜「晴天」给一首叫《93》的歌），后者返回加密串，
`json.Unmarshal` 直接失败。日志里只有一行 `cannot unmarshal string into ... neSearchResp.result`，
表面上「能播但搜不到」。

现在改用 `POST /api/cloudsearch/pc`（明文 JSON，字段是 `ar`/`al`/`dt`），
大批量搜索（歌单 >30 首）和歌词取 ID 都走这条。

**2. `devices.json` 写不进去（重启即丢设备绑定）**

上一轮把落盘改成「临时文件 + rename」的原子写。但 `devices.json` 在 docker
里是**单文件挂载**（`-v host/devices.json:/app/devices.json`），rename 到挂载点会
返回 `EBUSY`，于是只在内存生效、容器一重启设备绑定全丢。

现在 rename 失败会退化成原地覆盖写（牺牲一点原子性换可用性）。

## 三、环境变量

```ini
# 缓存回收
CACHE_TTL_DAYS=45              # 0 = 不按时间清理
CACHE_MAX_GB=20                # 0 = 不限总量
# 转码并发（2 核机器建议 2）
TRANSCODE_MAX_CONCURRENCY=2
# 运维页面应急口令，留空 = 只能登录访问（推荐留空，忘了密码再用）
ADMIN_PAGE_TOKEN=
# 每日转码额度（0 = 该维度不限）；匿名请求按 IP 算，登录/设备请求按账号算
TRANSCODE_DAILY_PER_IP=500
TRANSCODE_DAILY_PER_USER=0
# 音源自动降级：连续失败 N 次冷却 M 分钟（N=0 关闭）
SOURCE_DEGRADE_AFTER=5
SOURCE_COOLDOWN_MIN=5
# 导出 m3u 用的对外地址（走反代/域名时填 https://music.example.com），留空按请求推断
PUBLIC_BASE_URL=

# 数据备份：每天几点、留几份、是否开启
BACKUP_ENABLED=1
BACKUP_HOUR=3
BACKUP_KEEP=7
# 夜间预热：每次最多转几首（0 关闭）、几点跑
PREWARM_TOP_N=10
PREWARM_HOUR=4
# 夜间顺带预取歌词的曲目数（LrcAPI 冷查询慢，提前取好白天秒开）
PREWARM_LYRIC_N=15

# 歌词源 LrcAPI：默认用内置的 lrcapi 容器；清空则回落公共实例 https://api.lrc.cx
LRCAPI_BASE=http://lrcapi:28883
LRCAPI_AUTH=
# LRCAPI_ENABLED=0          # 整路关掉

# 外部音源 mp3api（YouTube，播放国内下架/被禁歌曲）
# 也可以不写这里，直接在 /tools 页面里填（保存即时生效，不用重启）
MUSIC_MP3API_KEY=
# MUSIC_MP3API_FIRST=1        # 临时把外部源提到整条链路最前
# MUSIC_MP3API_PREWARM=0      # 关掉后台预热

# 游客试听额度：未登录且无设备 Token 的请求按 IP 计数，0 = 不限
GUEST_DAILY_SONGS=20
# 网易云歌单自动增量同步：开关、每天几点、单次最多拉多少首
SYNC_ENABLED=1
SYNC_HOUR=5
SYNC_MAX_SONGS=500
```

登录/注册接口现在除了返回 token，还会写一个 HttpOnly 的 `session_token`
Cookie（SameSite=Lax，TLS 下自动加 Secure）。这样浏览器直接打开 `/status`、
`/remote` 时才有「身份」——页面导航是带不了 `Authorization` 头的。
退出登录会同时清 Cookie 并作废 token。

改完执行 `sudo docker compose up -d --force-recreate` 生效（env_file 变更不会热加载）。

## 四、接口速查

```
GET  /status                         运行状态面板
GET  /api/sources                    音源+缓存+转码状态（JSON）
GET  /api/lyric?song=&singer=        歌词 JSON（lines: [{time,text,trans?}]）
GET  /api/lyric?song=&singer=&trans=1      同上，优先取带翻译的版本（has_trans 表示是否真的有）
GET  /api/lyric?song=&singer=&format=lrc   纯 LRC 文本

POST /api/history/report              上报一次播放（听到 20 秒才算）
GET  /api/history?limit=              最近播放 + 最常听 + 累计次数
POST /api/history/clear               清空自己的播放记录
GET  /api/backup                      备份状态与现有备份列表
POST /api/backup/run                  立刻备份一次

GET  /manifest.webmanifest            PWA 清单
GET  /sw.js                           Service Worker
GET  /icons/*.png                     图标
GET  /lyrics?...                     歌词页
POST /api/auth/cookie                把 localStorage 里的 token 换成 Cookie（网关页自动调用）
GET  /remote                         设备遥控页（需登录）
GET  /tools                          歌单工具页（需登录）
GET  /api/user/playlists/export?id=&format=m3u|txt|json   导出歌单
POST /api/user/playlists/import-netease                   导入网易云歌单（auto_sync=true 同时开启每日同步）
GET  /api/user/playlists/sync-jobs                        我的自动同步任务
POST /api/user/playlists/sync-run                         立即同步（{job_id} 省略=全部）
POST /api/user/playlists/sync-toggle                      暂停/恢复（{job_id, enabled}）
POST /api/user/playlists/sync-delete                      取消同步关系（不删歌）
GET  /api/stats?days=14                                   播放趋势/榜单/时段分布
GET  /api/guest/quota                                     当前 IP 还能试听几首
POST   /api/lyric/custom                                  保存人工歌词（{song,singer,lrc}）
DELETE /api/lyric/custom?song=&singer=                    删除人工歌词，恢复自动源
GET    /api/settings/ext                                  外部音源状态（KEY 打码）
POST   /api/settings/ext                                  保存外部音源 KEY（空=清除）
POST   /api/settings/ext/test                             用当前 KEY 实探一次对方接口
GET    /api/settings/lyric                                歌词源地址与状态
POST   /api/settings/lyric                                改歌词源地址（仅站长）
POST   /api/settings/lyric/test                           用当前地址实拉一次歌词
POST /api/device/command             下发指令（需登录）
GET  /api/device/command/status      队列与最近回报（需登录）
GET  /api/esp32/poll?wait=20         设备长轮询（X-Device-Token）
POST /api/esp32/ack                  设备回报执行结果
```

## 五、运维命令

```bash
cd /vol1/docker/meowmusic
sudo docker compose ps
sudo docker compose logs -f --tail=100

# 改了源码后重建（源码在 ./src，改完直接 build）
cd src && sudo docker build -t meowmusic:patched .
cd .. && sudo docker compose up -d --force-recreate

# 回滚到官方镜像
sed -i 's|^MEOW_IMAGE=.*|MEOW_IMAGE=joe1280/meowmusic:latest|' .env
sudo docker compose up -d --force-recreate
```

数据备份：`.env` 里的 `MEOW_DATA_DIR` 下 `files/`（歌单/用户/设备）与
`devices.json`。`backup/` 目录里是历次改动前的 tar 包。

## 六、这一轮补的四个功能，细节说明

**1. 歌单导出 / 网易云导入**

- 导出：`/tools` 页每个歌单后面有 `m3u` / `txt` 两个按钮；`format=json` 给脚本用。
  m3u 的地址是 `${PUBLIC_BASE_URL 或请求 Host}/stream_live?...`，所以**要在外部播放器
  里用，记得先用公网地址访问一次下载**（用 127.0.0.1 打开得到的就是 127.0.0.1 的链接）。
- 导入：网易云 `api/v1/playlist/detail` 必须带 `n=1000`，不带只回前 10 首；
  导入按「歌名+歌手」去重，已有歌跳过；默认最多 200 首（`limit` 可调，上限 500）。
  导入只存元数据，播放仍走 `/stream_live` 多源回退，所以没有版权墙。

**2. 每日转码额度**

- 只统计「真的启动了 ffmpeg」的请求，命中缓存不计；计数落盘 `files/usage.json`，跨天自动归零。
- 匿名请求按 IP（默认 500 首/天），登录用户/设备按账号（默认不限）。
- 超限返回 `429` + `Retry-After`，日志里会打 `转码额度用尽`。
- 同一 NAT 后的多台设备共享一个 IP 额度，500 首/天约等于 25 小时音乐，正常用不到。

**3. 缓存时长回填**

- 转码成功后异步 `ffprobe` 一次，写 `files/cache/music/<歌手-歌名>/meta.json`（`duration` 秒）。
- 只有缓存命中且没有 meta 的老歌才会补测，同一文件不会重复测（有 in-flight 去重，
  ffprobe 全局只跑一个，避免抢 CPU）。
- 搜索结果里 `duration<=0` 的候选会自动用这里的值补上，前端进度条不再是空的。

**4. 音源自动降级**

- 实现在 `trySource()` 里：冷却中的源直接返回空结果、不发请求，所以 `helper.go`
  里十来个取链调用点一行都没改。
- 默认连续失败 5 次 → 冷却 5 分钟；冷却到期自动放行一次重试，失败立刻再冷却。
- 只在内存里，重启清零；`/api/sources` 会显示 `cooling_down` / `cooldown_until`，
  `/status` 面板的状态列显示「冷却中」。

## 七、第三轮补的四个能力（2026-11-22 晚）

**1. 网易云歌单「每天自动增量同步」**（`sync.go`）

- 导入歌单时勾上「每天自动同步」，就登记一条 job 到 `files/sync.json`
  （源歌单 ID + 本地歌单 ID + 累计新增/上次结果）；
- 每天 `SYNC_HOUR`（默认 05:00）拉一次源歌单，只把**新歌追加到末尾**；
  判重用「歌名+歌手」，老歌不动、顺序不变；
- **只加不删**：源歌单里删掉的歌，本地仍保留（避免误删自己后来又加回来的内容，
  行为完全可预期）；想清掉就删本地歌单或手动删歌；
- 开机 5 分钟后补跑一次（距上次不足 20 小时的跳过），NAS 不是 7×24 开着也不会漏；
- 单条失败不影响其它，失败原因写进 job 并在 `/tools` 页面标红；
- 删本地歌单会自动清掉挂在它上面的同步关系，不再每天报错；
- 手动触发：`/tools` 页面「立即同步」，或 `POST /api/user/playlists/sync-run`。

**2. 游客试听额度**（`guest.go`）

- 未登录、也没有设备 Token 的请求按来源 IP 计数，默认 **20 首/天**（`GUEST_DAILY_SONGS`，0=不限）；
- 只统计「从头开始的那次请求」：`Range: bytes=0-` 才计数，拖进度条、断线续传不重复算；
- 超限在 `/stream_live` 上直接 `429` + `Retry-After: 3600`，**不会启动 ffmpeg**；
- `/api/guest/quota` 给前端提前判断；`/status` 有「游客试听」卡片看今日消耗；
- 计数落盘 `files/guest.json`，跨天自动归零。

**3. 播放统计图表**（`stats.go` + `/status`）

- 在「真的听到 20 秒」那个上报点顺带记一份按天计数：`plays / transcode / guest / songs`，
  保留 90 天；另有 24 小时时段分布、歌手与歌曲累计 top 8（各留前 300 名，不会无限膨胀）；
- 页面用纯 CSS 柱状图渲染（不引任何图表库，断网也能显示）：
  蓝色柱=当天播放首数，柱内浅色段=其中真正转码的部分；
- `GET /api/stats?days=7|14|30`，`/status` 上「切换 7/30 天」按钮；
- 写盘 15 秒节流；统计失败不影响播放。

**4. HTTPS**：见 `deploy/https/README.md`

- 实测结论写进文档了：这台 NAS 的 80/443 被飞牛 nginx 占着（`nginx pid=3673`），
  Caddy 抢不到端口，Let's Encrypt HTTP-01 也验不过；
- **推荐路线**：飞牛 OS 自带的「反向代理 + Let's Encrypt」指到 `http://127.0.0.1:2233`，
  然后把 `.env` 的 `PUBLIC_BASE_URL=https://你的域名` 填上；
- 备选：`deploy/https/docker-compose.caddy.yml`（Caddy 上 8443 + 自签/DNS-01）；
- 反代三件套必须对：`Host`、`X-Forwarded-Proto`、关 `proxy_buffering`、长超时。

顺带修的：

- `/status?key=口令` 以前只能打开空壳页面（页面里的 `/api/sources`、`/api/stats` 全 401），
  现在这几个**只读**接口认 `ADMIN_PAGE_TOKEN`（写操作仍只认登录态）；
- 「最近备份」在进程刚启动时显示「从未备份」，现在会回落到磁盘上最新那份备份文件；
- `/status` 卡片文案压缩，避免「今日转码」那行折成三行。

## 八、第四轮：歌词纠错 / 分享链接 / 快捷键 / 外部源开关（2026-11-22 早）

**1. 外部音源 mp3api（YouTube）真正启用**（`settings_ext.go`）

- 代码里本来就有这条链路（搜索补位 + `/api/ext_audio` 代理 + 转码缓存），但
  `MUSIC_MP3API_KEY` 一直是空的，于是整条外部源被**静默跳过**——
  表现就是「Beyond《长城》搜出来全是 DJ 版」，看不出哪里不对；
- 现在 KEY 可以在 `/tools` 页面「外部音源（YouTube）」里直接填：存
  `data/files/settings.json`（0600），**保存即时生效**（`mp3ApiKey()` 每次现读），
  不用改 `.env`、不用重启；页面还有「测试连接」按钮，直接打一次对方 `/api/search`；
- 取值优先级：环境变量 `MUSIC_MP3API_KEY` > 页面保存的值（运维配置不会被页面顶掉）；
- 接口：`GET/POST /api/settings/ext`（KEY 只回打码后的 `5Wbd******LZT4`）、
  `POST /api/settings/ext/test`；都要求登录态。

实测（已配好线上 KEY）：搜「长城 / Beyond」，前 8 条全是 YouTube 外部源，
第一条就是《長城》Official MV，时长 296.5 秒；`/api/ext_audio?id=IOXoAvF6r_A`
返回 `206 Partial Content`，浏览器里 `readyState=4`、时长 4:56，能正常播。

**2. 歌词手动纠错**（`lyric_custom.go`）

- 歌词面板右上角加「✎ 纠错」：粘一版自己的 LRC 保存，**优先于 LRCLIB / 网易云**；
- 带 `[00:12.34]` 时间标签 = 逐行跟唱；纯文本也能存，按静态滚动显示；
- 存 `files/custom_lyrics.json`，键是「歌手|歌名」规范化后的字符串；全站共用一份
  （家里共用一个曲库，歌词对错是客观事实，不做成每人一份以免互相打架）；
- 「恢复自动」删掉覆盖，回到自动源；接口 `POST/DELETE /api/lyric/custom`，要求登录；
- 命中时 `/api/lyric` 返回 `source: "custom"`，按钮变成「✎ 已自定义」。

**3. 分享链接**（`full-app.html`）

- 播放器加 🔗：手机走系统分享面板，桌面直接复制
  `https://你的域名/?song=歌名&singer=歌手`；
- 打开这个链接（登录完成后）自动载入并播放，地址栏参数会被清掉，刷新不会重复触发；
- 浏览器会拦「没有用户手势的自动播放」，此时提示「点一下 ▶️ 就能听」，不再弹 alert。

**4. 键盘快捷键 + 音量记忆**

- 空格 播放/暂停、←/→ 快退快进 5 秒、↑/↓ 音量、M 静音、L 歌词、Q 队列、
  N/B 下一首/上一首、`/` 聚焦搜索、`?` 看提示；
- 输入框内、弹窗打开时不抢按键；音量与静音状态存 `localStorage`，下次打开还在。

**顺带修的两个上游布局 bug**

- `.modal` 本体样式缺失（上游只有 `.modal-content`），导致「创建歌单」弹窗一直
  平铺在页面最底部——补了 `.modal` / `.modal-content` / `.modal-title` / `.modal-buttons`；
- `.player-info` 是 `column`，四个按钮竖着排在歌名下面，把胶囊播放器撑得又高又空；
  改成 `row`，并让 `.player-controls` 变成 flex、`#audioPlayer` 固定最小宽度，
  「上一首 / 下一首」不再被挤到控制条上下两行。

## 九、第五轮：歌词源 LrcAPI + 每首歌标音源（2026-11-22 中午）

**1. 歌词源接上 LrcAPI**（`lyric_lrcapi.go`）

链路顺序：LRCLIB → 网易云 → **LrcAPI**（只在前两家都没结果时才问它，不拖慢正常请求）。

- 服务端自带一个 `lrcapi` 容器（`docker-compose.yml` 里的第二个 service，镜像
  `hisatri/lrcapi:1.5.2`），**不映射到宿主机端口**，只在容器网络内可达；
  主服务用 `LRCAPI_BASE=http://lrcapi:28883` 访问，所以没有公共接口的限流问题。
- 也可以用公共实例 `https://api.lrc.cx`（`.env` 里把 `LRCAPI_BASE` 清空即回落默认）；
  或者干脆在 `/tools` 页面「歌词源（LrcAPI）」里填地址，保存即时生效，不用重启。
- 接口兼容两套字段：新版 `/jsonapi` 返回 `lrc` + `lrc_ttml` + `score`，
  1.5.x 返回 `lyrics`，代码里都认；只给 TTML 的曲目会现场转成 LRC。
- **防张冠李戴**：它的模糊匹配很激进（搜不存在的歌也能给你塞一首回来），所以
  ① 用「歌名 + 歌手」规范化后复核，分数不够直接放弃；
  ② 过滤 `[ti:] [ar:] [hash:]` 这类元信息行；
  ③ 「纯音乐，请欣赏」这类占位文本不算歌词；
  ④ 有效歌词行少于 3 行一律当没找到。
- **冷查询慢**：首次命中要 8~12 秒（它要去酷狗/Apple 现拉），命中后它自己会缓存，
  二次只要 1ms。为此补了两个缓冲：
  - 「确实没歌词」记 12 小时（`files/cache/lyrics/_miss_*.json`），
    不会每播一次就空等十几秒；
  - 夜间预热（`PREWARM_LYRIC_N`，默认 15 首）在 04:00 把歌单里的歌提前取一遍歌词，
    白天就是读缓存。
- 配置：`LRCAPI_BASE` / `LRCAPI_AUTH`（自建实例设了 `--auth` 才需要）/
  `LRCAPI_ENABLED=0` 可整路关掉；`/tools` 页面能改，但**只有站长（最早注册的账号）**
  能改地址——地址决定服务器替你去访问哪台主机，不能让普通账号随便指。
- 接口：`GET/POST /api/settings/lyric`、`POST /api/settings/lyric/test`

**2. 每首歌都标出音源（搜索结果 / 歌单 / 最近 / 队列）**

- `/api/search` 现在给每条结果带上真实来源：`kuwo` / `netease` / `qq` / `kugou` /
  `ext`(YouTube)，以前只有「是不是外部源」这一个布尔信息；
- 前端统一渲染成「色块 logo + 源名」的小标签：酷我蓝、网易云红、QQ 绿、酷狗深蓝、
  YouTube 红（播放键图标），标题后面一眼看出这首歌是从哪来的；
- 音源会跟着数据走：加进歌单时存 `source`、上报播放历史时存 `source`、
  队列存 localStorage 时也带着，所以下次打开还能看到；
- 历史记录里来源只允许这几个白名单值，前端塞别的字符串会被丢掉。

## 十、第六轮：/status 顶部「正在播放」（2026-11-22 早上）

**问题**：`/status` 上全是「过去时」——按天的播放趋势、累计榜单、音源健康，
但看不到**此刻在放哪首**。听完才算一次播放，刚点开的歌在统计里根本不存在。

**做法**（`nowplaying.go` + `status.html` + `full-app.html`）：

- 播放器心跳：`<audio>` 的 `play` / `pause` / `ended` 立刻上报，
  `timeupdate` 每 8 秒最多打一次 `POST /api/now`，带 title / artist / cover /
  source / duration / position / playing / client；
- `client` 是 `localStorage` 里的随机 id，同一账号开两个标签页不会互相覆盖；
- 服务端纯内存 `map[userID|client]*nowEntry`，最多 64 条，**30 分钟没心跳自动清掉**
  （关页面、断网都不留僵尸行），进程重启清零；
- 「正在播放」的判定：`playing=true` 且 45 秒内有心跳（`fresh`），
  暂停状态仍显示但变灰、显示「已暂停」，进度条停在原位；
- `GET /api/now` 返回列表：正在放的排前面，带用户名反查（`usernameByID`）、
  音源徽章、`mm:ss / mm:ss` 进度、心跳时间（「3 分钟前」）；
- 页面每 5 秒刷一次这一块（其余面板仍是 10 秒），封面缺失时用 ♪ 占位。

**接口**：`POST /api/now`（需登录）、`GET /api/now`（登录或 `?key=ADMIN_PAGE_TOKEN`）。

## 十一、第七轮：汽水音乐源（2026-11-22 下午）

**为什么不是自己啃协议**：汽水音乐没有公开接口，PC/H5 接口要 `x-helios` / `x-medusa`
签名，音频还是加密的（spade 加密，要先用 `spade_a` 解出 AES key 再 AES-CTR 解）。
自己实现等于每月跟着改一次，所以直接复用同机的开源项目
[`guohuiyuan/go-music-api`](https://github.com/guohuiyuan/go-music-api) 当 sidecar。

**架构**

- `docker-compose.yml` 新增 `gomusic` 服务（镜像 `guohuiyuan/go-music-api:latest`，
  **不映射宿主机端口**，只在容器网络内 `http://gomusic:8080`）；登录态挂在
  `data/qishui/cookies.json`，容器重建不用重扫。
- 主服务 `qishui.go`：
  - 搜索 `GET /api/v1/music/search?q=&type=song&sources=soda`
  - 取链 `GET /api/v1/music/url?id=&source=soda`（解密在 sidecar 里做，返回明文 m4a）
  - 打分复用酷狗同款规则（歌名 / 歌手 / 版本噪声 / 时长），阈值 60 分
- 播放接进 `requestAndCacheMusic` 的回退链，位置在 **GDStudio 之后、mp3api 之前**
  —— 汽水未登录只有 60 秒试听，不能顶掉能放完整版的源，但比 mp3api 冷启动 13 秒快。
- 搜索聚合 `searchFromAPI` 里加了一路兜底（酷狗之后），但**混在「全部」里基本排不到前面**
  （酷我曲库太全），所以前端搜索框下加了「音源」筛选条：全部 / 酷我 / 网易云 / QQ /
  酷狗 / 汽水 / YouTube。选「汽水」时后端 `?source=qishui` 只走这一路。

**未登录的兜底**：汽水搜索接口要登录态，未登录返回空；但热门榜和官方榜单是公开的，
所以把 `ranking/hot` + 前三个榜单抓成「曲库快照」缓存 30 分钟，搜索没结果时在本地按
歌名/歌手匹配（搜《天龙八部之宿敌》能搜到、能出 60 秒试听）。

**登录（关键）**：`/tools` 页面底部「汽水音乐（抖音曲库）」面板 →

- 点「生成登录二维码」→ 后端 `POST /api/qishui/qr` 转调 sidecar 的
  `POST /api/v1/system/qr_login/soda`，把 `data:image/png;base64` 直接贴到 `<img>`；
- 页面每 2.5 秒轮询 `GET /api/qishui/qr?key=`，只要不再是 waiting 就查一次
  `/api/qishui/status` 看 `logged_in`；
- **必须用抖音 App 扫**（汽水 App 扫可能不过），扫完 sidecar 自己写 cookies.json。

**配置**：`QISHUI_BASE`（默认 `http://gomusic:8080`）、`QISHUI_ENABLED=0` 整路关闭、
`GOMUSIC_IMAGE` 换镜像。

**踩坑**

- `maskSecret` 和 `settings_ext.go` 里的同名函数冲突，编译期才报错——直接复用那个；
- `sudo docker build` 走 paramiko 的 pty 会卡在密码提示，必须
  `echo '密码' | sudo -S docker build ... > 日志 2>&1`，且重定向要放外层；
- 只填关键词时 `song` 位是搜索词，歌手为空 → 打分函数不会走歌手加权，
  所以「搜歌手名」在汽水筛选里命中率低，正常搜歌名即可。

## 十二、汽水音乐：登录为什么一直失败（2026-11-22 傍晚实测）

扫码能出、手机也能扫到、点确认后服务端状态却永远停在 `scanned`，
cookie 一直不落盘。逐项排查后的结论：

| 探测项 | 结果 | 说明 |
|---|---|---|
| 本机出口 IP | 东京，`AS31898 Oracle` | 汽水是字节国内产品 |
| `GET /api/v1/ranking/hot?source=soda` | **200，正常返回榜单** | 榜单是公开接口，境外 IP 也能读 |
| `GET /api/v1/music/search?...&sources=soda` | `{"songs":null}`，**始终空** | 搜索必须带登录态，跟 IP 无关 |
| 注入抖音 web cookie 后 | 搜索**仍然空**；并且榜单开始回 `ERR_REQUEST_FORBIDDEN / 1000006` | 抖音域的 sessionid 不能直接给汽水用；带着无效 cookie 反而触发风控 |
| 清掉 cookie | 榜单恢复正常 | 证明确实是 cookie 触发的风控 |
| 二维码轮询 | 停在 `scanned`（`cached_status/rate_limited`） | 换 ticket 那步没走通 |

**所以「是不是只能国内 IP」——不完全是。** 榜单境外可读，说明网络层没被整段封；
真正卡住的是**登录换票**：手机端确认了，但 sidecar 用同一个 key 去换 ticket 时
拿不到 `confirmed`。可能原因按可能性排序：

1. 换票请求带设备/IP 校验，境外 IP 被拒（换国内机器大概率能解决）；
2. sidecar 轮询被上游限流后一直返回缓存的 `scanned`（我们前期 2 秒一次轮询确实招来了
   `rate_limited`，后期改 8 秒也没再给用户扫第二次，没排除干净）；
3. 抖音号没开通汽水，确认页其实是「去开通」。

**换到国内机器后按这个顺序验**（不用改代码）：

```bash
# 1. 先确认新机器 IP 归属
curl -s https://ipinfo.io/json | head -5

# 2. 榜单 + 搜索都通才算真活
curl -s 'http://172.19.0.4:8080/api/v1/ranking/hot?source=soda&limit=1'
curl -s 'http://172.19.0.4:8080/api/v1/music/search?q=%E6%99%B4%E5%A4%A9&type=song&sources=soda&limit=1'

# 3. 搜索还是空 → 去 /tools 扫码，页面轮询间隔已改 8 秒（别用 2 秒，会被限流）
# 4. 登录后 status 的 usable / searchable 都为 true，badge 才是完整版
```

**代码现状（已编译通过，未部署到现网镜像）**

- `qishui_admin.go`：`qishuiProbe()` 一次探测两件事，缓存 10 分钟 ——
  `usable`（榜单能读，链路活着）、`searchable`（有登录态，能搜全库）；
  `/api/qishui/status` 两个字段都返回，`reason` 给出人话原因。
- `theme/full-app.html`：`detectQishui()` 在页面加载时查一次状态 ——
  `usable=false` 直接把「汽水」筛选项摘掉（避免点了必然空）；能连上但没登录时给
  tooltip 提示「只能搜到榜单里的歌」。
- 镜像 `meowmusic:check` 是这次编译校验的产物，现网仍跑 `meowmusic:patched`（旧逻辑）。

**已知能力边界（不登录）**：只能搜到榜单曲库快照（`ranking/hot` + 前几个榜单，
缓存 30 分钟本地匹配），播放只有 **60 秒试听**（`/api/v1/music/url` 返回的音频
ffprobe 实测 duration=60；CDN 境外可访问，能正常转码）。

## 十三、还可以继续做的

1. **HTTPS 真正落地**（需要你有一个域名 + 在飞牛面板点几下，见 `deploy/https/README.md`）；
2. 多用户共享歌单（`is_public` 字段早就有，但没有浏览别人歌单的入口）；
3. 设备端离线播放列表缓存（ESP32 侧存最近 N 首，断网还能放）；
4. 转码失败自动换源重试的可视化（现在日志里有，页面看不到）；
5. 外部源的可选策略（哪些歌优先走 YouTube、外部源单独限流）；
6. 换到国内服务器后：汽水扫码登录 + 验证 `searchable=true`。

## 十四、镜像发布到阿里云 ACR + 跨海推送提速（2026-11-22 晚）

把 `meowmusic` 镜像推到阿里云个人版容器镜像服务，作为换机器时的分发通道。

- 地址：`crpi-wv2uaegt8g2y0g3c.cn-guangzhou.personal.cr.aliyuncs.com/joeloveqt-project/meowmusic`
- 命名空间 `joeloveqt-project`，仓库 **公开**（pull 不用登录，push 才要 `docker login --username=aliyun1109508239`）
- 标签：`latest`、`20261122-arm64`（最新代码，digest `a47edefb…`）、`patched-20261122-arm64`（现网回滚点，`970ab513…`）
- 只有 `linux/arm64`：aarch64 机器直接拉；x86 机器必须 `buildx --platform linux/amd64` 重建

三个坑：

1. **首次 push 报 `denied: requested access to the resource is denied`**，不是权限问题，是
   **仓库不存在**。个人版必须先到控制台「仓库管理 → 创建仓库」（或给命名空间开「自动创建仓库」），
   仓库类型选**本地仓库**，别绑代码源。
2. **命名空间不等于登录账号**。登录名是 `aliyun1109508239`，仓库路径里的是 `joeloveqt-project`，
   两个不一样，tag 写错会一直 denied。
3. **跨海推送慢到不可用**：东京 → 广州 47MB 的 ffmpeg 层只有 25KB/s。`ss -tin` 诊断出
   RTT 176ms、重传率 12%、`cwnd:2` —— 典型 CUBIC 在丢包链路上崩掉。`modprobe tcp_bbr` +
   `sysctl -w net.ipv4.tcp_congestion_control=bbr` 之后飙到 3.5MB/s，几十秒推完。
   以后这台机器往国内推任何大文件（镜像、备份）都可以先开 BBR。

顺手记一个操作纪律：`pkill -f "docker push"` 会匹配到自己的 `sh -c` 命令行，把自己也杀掉
（日志里留下 `LATEST_EXIT=143`），要么用 `pkill -f '^docker push'`，要么按 PID 杀。

## 十五、CI 直推阿里云 ACR + 部署链路打通（2026-11-22 夜）

结论先说：**以后不要再从东京硬推镜像了**，走 CI 更快也更稳。

- `main` 一 push，`.github/workflows/docker-build-deploy.yml` 用 QEMU 构建
  `linux/amd64,linux/arm64` 双架构，一次推 Docker Hub + GHCR + 阿里云 ACR
- ACR 上的 `latest` 因此是多架构 OCI index，x86 / ARM 机器都能直接拉
- 部署任务 `Deploy to NAS` 优先从 ACR 拉（公开仓库，免登录），失败才回落 Docker Hub、GHCR

修掉的三个部署坑：

| 现象 | 真因 | 修法 |
|---|---|---|
| `Pull 最新镜像` 恒失败且耗时极长 | `compose pull` 连 `lrcapi`/`gomusic` 一起拉，这两个在 Docker Hub，目标机连不上 | 改成 `pull meow-music` |
| `Error saving credentials: mkdir /home/joe1280: permission denied` | runner 的 `joe1280` 没有 home 目录，docker login 写不了 `~/.docker` | job 级 `DOCKER_CONFIG: ${{ github.workspace }}/.docker` |
| 单一源失败即整体失败 | 无回落 | ACR → Docker Hub → GHCR 三级，每级重试 3 次 |

还有一点容易搞混：**部署目标 runner `nas-220` 不是东京那台 VPS**。VPS 内网是
`10.0.0.147`，runner 在 `192.168.2.220` 的另一个局域网里；那台机器
`registry-1.docker.io` 直接 `connection reset`，所以 ACR 是它唯一能用的源。

实测一次全绿：build（双架构）→ 三处推送 → NAS 从 ACR 拉取（第 1 次就成功）→
容器 `Up 7 seconds (healthy)`。

## 十六、汽水音乐：登录待境内机器，页面按钮保持常显（2026-11-22 深夜）

结论：**这台机器不尝试汽水登录，但也不屏蔽这个音源** —— 按钮一直显示在页面上，
境内机器登录后立刻就能用；境外机器搜索照样能出结果，播放也能用（见下面的复测）。

### 最终处理（2026-11-23 01:20）

- `.env` 保持 `QISHUI_ENABLED=1`，`qishui-proxy` + `gomusic` 两个容器都跑着
- 改了 `theme/full-app.html` 的 `detectQishui()`：**探测失败也不再 `chip.remove()`**，
  只在 tooltip 里说明当前状态（可用 / 未登录只搜榜单 / 该机已关闭 / 连不上）
- 镜像 `meowmusic:qishui1`（`.env` 的 `MEOW_IMAGE` 指向它）
- 实测：首页 HTML 里 `data-src="qishui"` 存在；`/api/search?...&source=qishui` 搜
  「光辉岁月」第一条就是 Beyond 原唱

### 2026-11-23 01:50 复测：未登录其实也能播全长，但 CDN 节点随机

用户反馈「汽水能播了」，实测确认，**和登录无关**（`cookies.json` 一直是 `{}`）：

| 观测 | 数据 |
|---|---|
| 取链 | `GET /api/v1/music/url?id=..&source=soda` 返回 `*.douyinvod.com` 直链 |
| 体积 | 194s 的歌 **6,243,061 字节 ≈ 257kbps**，是完整曲目，不再是 30 秒试听 |
| 节点分布（6 次采样） | `v5` / `v5-ex` / `v3` / `v26-luna` 全部 200、6.24MB；**`v11-luna` 整段超时（000）** |
| 失败表现 | 命中不通节点时 ffmpeg `exit status 146`，日志 `Transcode error` |

也就是说：汽水对未登录用户放了完整版，但直链按请求**随机分配 CDN 节点**，
境外约 1/3 概率落到不通的 `v11-luna` 上。之前判定「只能试听」是早期策略，已过期。

**已加自动换节点重试**（`api.go` 的 `/stream_live`）：

```
取链 → ffmpeg 转码 → 失败且源是 qishui → qishuiRefetchURL 重新取链（换节点）→ 最多 3 次
```

- `qishuiRefetchURL` 只重跑汽水自己的搜索+取链（2 次容器内 HTTP），不重跑整条多源链
- `retryableTranscodeSource` 只对 `qishui` 返回 true：其它源直链是固定对象存储地址，重试无意义
- 实测 8 轮清缓存重取，**8/8 成功**；日志里能看到「第 1 次 → 转码失败 → 重新取链 → 第 2 次 → 成功」

注意：汽水的排序仍在 `mp3api` 之前、国内主源之后。所以用户搜「汽水」筛出来的歌，
**实际音频常常来自网易云/酷我**——播放走的是全局多源兜底，汽水排第 10 位。
日志里的 `[Security] [qishui] 命中` 才是真的轮到汽水。

### 为什么登录一定失败

22:30–23:47 连续测了 6 个二维码（每个有效期 5 分钟，`expires_at` 可以验证），
情况完全一致：

| 现象 | 数据 |
|---|---|
| 码能生成 | `POST /api/v1/system/qr_login/soda` 每次都 200，返回 key + base64 PNG |
| 状态能查 | 每 3 秒查一次，连查 40 次全部 200 |
| 但永远 `waiting` | 40 次里 0 次 `scanned`、0 次 `confirmed`，cookies.json 始终 `{}` |
| 不是限流 | 全过程 `message:"success"`，没有 `rate_limited`（只有一次 18 秒内连建 3 个码触发了风控，那一个 token 后才出现 `rate_limited`） |

也就是说：**扫码动作根本没有传到汽水的服务端**。手机侧无论怎么扫，服务器这边读到的
永远是「等待扫码」。判断是境外出口 IP（东京 Oracle AS31898）在扫码换票环节被拒，
不是代码问题、不是轮询频率问题。

顺带记两个坑：

- `POST /api/v1/system/cookies` 传任意 JSON 会**原样合并**进 cookie 存储。
  测试时传了 `{"source":"soda","cookie":"x"}`，结果 `cookies.json` 变成
  `{"cookie":"x","source":"soda"}`，汽水后续请求全带着假 cookie。
  恢复方法：`echo '{}' > data/qishui/cookies.json` 再 `docker restart gomusic`。
- 建码频率要克制：18 秒内连建 3 个，汽水会对新 token 回 `rate_limited`，
  那个 token 就废了，得等冷却再建。

### 如果需要临时把汽水整路关掉

```bash
# 1. .env 追加（env_file 会带进容器）
QISHUI_ENABLED=0

# 2. 重建主服务（配置才生效）
cd /vol1/docker/meowmusic && sudo docker compose up -d meow-music

# 3. 停掉只为汽水存在的两个容器（省内存）
sudo docker stop qishui-proxy gomusic
```

### 将来在境内机器上重新开启

1. `.env` 把 `QISHUI_ENABLED=1`（或删掉这一行，默认就是开）
2. `docker compose --profile qishui up -d qishui-proxy gomusic`
3. 浏览器打开 `/tools` → 「汽水音乐」面板 → 生成二维码 → **抖音 App 扫码并点确认**
4. 看 `data/qishui/cookies.json`：从 `{}` 变成带 `sessionid` 的对象 = 登录成功；
   再拿一首 300 秒的歌验证 `/api/v1/music/url` 返回的文件大小（未登录只有约 30 秒）

### 备选方案：直接注入 cookie

不改代码就能绕过扫码 —— go-music-api 支持热更新 cookie：

```bash
curl -X POST http://127.0.0.1:8080/api/v1/system/cookies \
  -H 'Content-Type: application/json' \
  -d '{"soda":"sessionid=xxx; sessionid_ss=yyy; ..."}'
```

cookie 从自己电脑的浏览器拿：打开 `https://bff-pc.qishui.com` → 抖音扫码登录 →
F12 → Application → Cookies → 复制该域下全部（关键是 `sessionid`）。
注意 2026-11-22 傍晚试过注入**抖音网页版**的 cookie，结果汽水直接回
`ERR_REQUEST_FORBIDDEN/1000006`（风控），所以必须是**汽水站自己**的 cookie。

## 十七、新增「喜马拉雅」音源（2026-11-23 凌晨）

需求来源：用户提出接入 `sld272/Ximalaya-Downloader-Next` 那套思路。那个项目的定位是
「用你自己账号权限批量下载」，核心是浏览器登录 + xm-sign 签名 + APK 协议，代码量很大
（sign/extractor、source_pc、source_http、native_signer 全套）。本站只需要「搜得到 + 放得响」，
所以**没有照搬它的签名链路**，而是找到了网页版匿名就能用的两个公开接口。

### 实际用的接口（2026-11-23 01:50 在东京机器实测）

| 用途 | 请求 | 说明 |
|---|---|---|
| 预热 cookie | `GET https://www.ximalaya.com/` | 拿 `HWWAFSESID`，不预热搜索直接 **400** |
| 搜索 | `GET /revision/search?core=track&kw=<关键词>&page=1&rows=N&spellchecker=true&condition=relation&device=web` | 返回 `data.result.response.docs[]`：`id / title / nickname / duration(秒) / is_paid / cover_path` |
| 取链 | `GET /revision/play/tracks?trackIds=<id>` | 返回 `data.tracksForAudioPlay[0].src`（m4a），**只有免费内容才有 `src`** |

实测数据：免费节目《周杰伦歌友会》5330 秒，直链 43MB（约 64kbps m4a），
`/stream_live` 转码后 20.8MB / 5331 秒，`http=200`；付费节目（三体广播剧）取链没有 `src`，
返回 `403 该音频需要喜马拉雅会员，本站无法播放`。

### 集成方式（和汽水不同的两点）

1. **只在「喜马拉雅」筛选按钮下出现**。喜马拉雅的内容是有声书/播客/广播剧，和音乐源
   完全不重叠，混进「全部」会把搜索结果冲成播客列表，所以 `searchFromAPI` 里没有它。
2. **播放带 `xid`**。搜索结果里的播放地址是
   `/stream_live?song=..&singer=..&xid=<trackId>`，`/stream_live` 看到 `xid` 就按 id 直取，
   **不走多源链路**。原因：这些播客标题常常就是歌名（比如「晴天｜周杰伦」），
   靠「歌名+歌手」二次解析必然张冠李戴。

改动清单：

- 新增 `ximalaya.go`：带 cookiejar 的 client + 首页预热（cookie 30 分钟一换，400/403 自动重来）、
  `XimalayaSearchLimit()`、`XimalayaTrackSrc()`
- `search.go`：`livePlayURLX()`（带 xid 的播放地址）、`searchOnlySource` 增加 `case "ximalaya"`
- `api.go`：`/stream_live` 读取 `xid`，命中时改为按 id 取链
- `history.go`：来源白名单加 `ximalaya`
- `theme/full-app.html`：筛选栏加「喜马拉雅」按钮 + `SRC_META` 加橙色播放图标徽标

开关：`XIMALAYA_ENABLED=0` 可整源关闭（默认开）。会员内容不绕过付费墙，这是上游本身的限制。

## 十八、新增「5sing」音源（2026-11-23 凌晨）

补充源，来源参考 CharlesPikachu/musicdl 的 `fivesing.py`，**按接口自己重写在 Go 里**
（musicdl 是 PolyForm Noncommercial 许可、README 禁止捆绑再分发，只做参考不可 vendored）。

5sing 是原创/翻唱/伴奏社区，内容全是用户上传，没有付费墙，和唱片公司曲库不重叠。

### 接口（2026-11-23 东京机器实测）

| 用途 | 请求 | 说明 |
|---|---|---|
| 搜索 | `GET http://search.5sing.kugou.com/home/json?keyword=<kw>&sort=1&page=<n>&filter=0&type=0` | 返回 `list[]`：`songId / songName（含 <em> 高亮标签）/ singer / typeEname`；`pageInfo.totalPages` 控翻页，每页 10 条 |
| 取链 | `GET http://mobileapi.5sing.kugou.com/song/getSongUrl?songid=<id>&songtype=<yc|fc|bz>` | 返回 `data.squrl / hqurl / lqurl`（mp3 https 直链）+ `*_backup` 备用域名；`code=1000` 为成功 |

两个坑：

1. **搜索必须走 http**。`search.5sing.kugou.com` 的证书只签给 `*.kugou.com`，
   https 直接 `SSL: CERTIFICATE_VERIFY_FAILED / Hostname mismatch`，握手就失败。
   返回体是公开 JSON，不含任何凭据，走 http 没有额外风险。
2. **搜索列表没有 duration、没有封面**。时长留 0 交给 `cachedDuration` 兜底；
   `getSongUrl` 里的 `user.I` 是上传者头像不是专辑图，不拿它当封面。

实测：搜索 `原创` 返回 10 条；取链 `songid=2104925&songtype=yc` → `squrl` 8.16MB mp3，
直链 `206 audio/mpeg`，境外可完整播放（这点和汽水不同，5sing 没有节点问题）。

### 集成方式

- 播放地址带 `xid`，格式 **`fs:<songId>:<typeEname>`**。取链必须 id + type 一起给，
  只给 id 取不到、只给歌名更认不出来（歌名是用户自己起的）。
- `/stream_live` 按前缀分流：`fs:` 开头走 `FiveSingTrackSrc`，纯数字仍走喜马拉雅。
- 搜索结果里混着伴奏（`typeEname=bz`），默认过滤——本站不标注「伴奏」，
  用户点进去会听到没有人声的东西；但如果过滤后为空（用户搜的就是伴奏），保留原样。
- `search.go` 抽出 `addID()` 闭包，喜马拉雅和 5sing 共用（都是「带 id、封面可选」）。

改动清单：

- 新增 `fivesing.go`：`FiveSingSearchLimit()`、`FiveSingTrackSrc()`、`fiveSingTrackXID()`、`FIVESING_ENABLED` 开关
- `search.go`：`searchOnlySource` 增加 `case "fivesing"`，抽出 `addID` 闭包
- `api.go`：`/stream_live` 的 `xid` 分支改为按前缀分流（`fs:` / 纯数字）
- `history.go`：来源白名单加 `fivesing`
- `theme/full-app.html`：筛选栏加「5sing」按钮 + `SRC_META` 加橙色音符徽标
- `deploy/.env.example`：`FIVESING_ENABLED` 说明

开关：`FIVESING_ENABLED=0` 整源关闭（默认开）。和喜马拉雅一样，**不进「全部」、不进多源兜底链**。

## 十九、汽水第二条取链路径：网页分享页（2026-11-23 凌晨）

起因：参考 CharlesPikachu/musicdl 的 4 条汽水取链路子。其中「官方非 VIP」那条
是扒网页版分享页 `music.douyin.com/qishui/share/track?track_id=` 里的
`_ROUTER_DATA`，不经过 go-music-api，也不必处理加密音频。

### 实测结论（东京机器，2026-11-23 04:30）

| 维度 | 池 A：gomusic `/api/v1/music/url` | 池 B：分享页 `_ROUTER_DATA` |
|---|---|---|
| 格式 / 码率 | mp3，约 257kbps | mp4 容器 + AAC，约 130kbps |
| 常见节点 | v3-luna / v5-ex-luna | v11-luna / v26-luna / v5-se-* / v95-se-* |
| 单次可达率（境外） | 5/5 全部 206 | 3/6（桌面 UA）、1/6（手机 UA） |
| 依赖 | 需要 gomusic 容器解密 | 无依赖，纯 HTTP |

**所以它不能替代池 A**：码率只有一半，而且命中不通节点的概率更高。
价值在于两个池的节点分配**完全独立** —— A 落到 v11 时，B 往往能给 v5-se / v26 / v95。
定位：**兜底池**，不是替代品。

### 内容长度的重要更正

之前笔记写的「未登录也给全长（6.24MB / 194s）」只对**免费曲**成立。
付费曲两个池都只给约 60 秒试听片段：

| 曲目 | level | 总时长 | previewStart → previewEnd | 实得 |
|---|---|---|---|---|
| 红色高跟鞋 | pay | 206.6s | 149.2 → 206.6 | 57.4s / 1.8MB(mp3) / 0.9MB(m4a) |
| 天龙八部之宿敌 | pay | 263.3s | 163.4 → 223.4 | 60s |
| 小半 | pay | 297.2s | 143.8 → 173.9 | 30s |
| 烂奇迹 | free | 207.0s | 0 → 207.0 | 全长 207s |

`group_playable_level` / `previewStart` / `previewEnd` 都在
`loaderData.track_page.audioWithLyricsOption` 里，换取链路径解决不了。

### 接口

```
GET https://music.douyin.com/qishui/share/track?track_id=<id>
UA:  桌面 Chrome（关键！手机 UA 下 6 次有 5 次分到 v11-luna）
→ 页面里 window._ROUTER_DATA = {...}
→ loaderData.track_page.audioWithLyricsOption.url
```

解析坑：`_ROUTER_DATA` 后面是「一段完整 JSON + `</script>`」，
不能 `json.Unmarshal` 整段（会 `Extra data`），要用 `json.Decoder` 读第一个对象。

### 探活（qishuiProbeURL）

`Range: bytes=0-1` + 3 秒超时，200/206 视为可达。
起因：命中 v11-luna 是「TCP 连得上但整段无响应」，ffmpeg 要等十几秒才 exit 146，
代价太高。探活 3 秒内就能下结论，直接换下一个候选。

### 改动

- `qishui.go`
  - 新增 `qishuiShareURL(id)`：扒分享页、解析 `_ROUTER_DATA`
  - 新增 `qishuiProbeURL(link)`：2 字节 Range 探活，3s 超时
  - 新增 `qishuiPickURL(id)`：先池 A 后池 B（最多 3 次换节点），每条探活后再返回；
    全程 12 秒预算，全部不通则返回第一条非空链接交给 ffmpeg 试
  - `qishuiResolve` 改用 `qishuiPickURL`，因此 `QishuiSearch` 与 `qishuiRefetchURL`
    自动获得双池能力
  - 修正文件头注释里「未登录给全长」的错误结论

### 实测（东京）

```
分享页 7068138081588611073 → v11-luna（探活失败，符合预期）
分享页 7705267063577511990 → v95-se-zjwztc-luna（探活通过）
qishuiPickURL 池A 失败（宿主机无 gomusic）→ 分享页池第 2 次命中 → 探活通过
```

### 补充：分享页池改成并发取链（同日）

串行探测在最坏情况下要 3 ×（1s 取页 + 3s 探活）≈ 12 秒，加上池 A 那 3 秒，
整条取链能拖到 20 秒以上。改成**并发 3 路**：三个请求同时打分享页、同时探活，
取下标最小的可达结果。最坏从约 12 秒压到约 4 秒。

线上实测（东京，红色高跟鞋，每轮清缓存）：

| 轮次 | 池A | 实际耗时 | 说明 |
|---|---|---|---|
| 1 | v5-ex-luna 探活通过 | 3.6s | 一次转码成功 |
| 2 | v3-luna 探活通过 | 3.3s | 一次转码成功 |
| 3 | 探测不通 | 9.9s | 分享页池**并发第 1 路**命中 v95-se，一次转码成功 |
| 4 | 探测不通 | 10.5s | 分享页池**并发第 1 路**命中 v5-se-ex-mc，一次转码成功 |

改之前同样的池A 失败场景要走「ffmpeg 转码失败 → 重取链 → 第二次转码」，实测 22.8s。
现在 4/4 都是第一次转码就成功。

## 二十、CI 拆分：构建与部署分离（2026-11-23 凌晨）

### 问题

`deploy` job 的触发条件原本是：

```yaml
if: always() && needs.build.result == 'success' &&
    (github.event_name == 'push' || inputs.deploy)
```

也就是**每次 push 都排一个 Deploy to NAS**。它跑在 NAS 的自托管 runner 上，
NAS 关机 / runner 没起来时，这个 job 会**无限期 queued**，并且占住本 workflow 的
并发组 `Build and Deploy Docker Image-refs/heads/main` 不放。

后果：之后每次 push 的 run 都卡在 `pending` 且 `jobs` 为空 —— 镜像也不出了。
`concurrency.cancel-in-progress: true` **救不了**：它只取消「正在跑」的 run，
对「已排队但没分配到 runner 的 job」无效，只能 `POST /actions/runs/<id>/force-cancel`。
这个坑在 2026-11-23 凌晨连续踩了三次（45065295678、45067412819、45070791491）。

### 改法

拆成两个 workflow，各用各的并发组：

| 文件 | name | 触发 | 干什么 |
|---|---|---|---|
| `.github/workflows/docker-build-deploy.yml` | `Build and Push Docker Image` | push main / 手动 | 只构建 + 推三仓库 |
| `.github/workflows/deploy-nas.yml` | `Deploy to NAS` | **仅手动** | 拉镜像 + 重建容器 + 健康检查 |

细节：

- `deploy-nas.yml` 的并发组是 `deploy-nas`、`cancel-in-progress: false`，
  连续手动触发时排队而不是互相取消（避免容器重建到一半被掐）。
- 新增输入 `image_tag`（默认 `latest`），可以精确部署 `sha-xxxxxxx`。
- 拉取顺序仍是 ACR → Docker Hub → GHCR，每个源重试 3 次；只拉 `meow-music`。
- workflow 的 `name` 也改了，所以新 run 的并发组名不同 —— 历史上那些卡住的旧 run
  不会再拖累新构建（这一点是意外收益）。
- 想恢复「push 后自动部署」：把 `deploy-nas.yml` 顶部注释掉的 `workflow_run` 块加回来即可。
  注意 workflow_run 触发的 run 里 `GITHUB_SHA` 是默认分支最新提交、不是触发它的那个 commit，
  checkout 必须显式用 `github.event.workflow_run.head_sha`，否则 compose 文件与镜像版本对不上。

### 顺带的说明

`test-and-build.yml` 是上游 ESP32 项目的构建流程（`runs-on: self-hosted`，无标签），
只在手动触发的发版场景用，和本项目的镜像链路无关，未改动。
