# MeowMusicServer · 多音源增强版

给嵌入式设备（ESP32 等）用的局域网音乐流媒体服务器。

**核心改造：把原来单一的音源换成了「酷我波点 → 网易云 → GD Studio」三层自动回退链路，并加了一套选曲打分算法，解决点歌时最常见的两个问题——「搜不到」和「放错版本」。**

实测 58 首中英文/粤语/日语歌曲，全部命中正确歌手与正确时长，取链耗时 **0.5–1.4 秒**。

本项目基于 [OmniX-Space/MeowMusicServer](https://github.com/OmniX-Space/MeowMusicServer) 修改，上游说明见 [README_UPSTREAM.md](README_UPSTREAM.md) / [README_zh-CN_UPSTREAM.md](README_zh-CN_UPSTREAM.md)。

---

## 为什么需要改造

原版服务器依赖第三方公益音源 API，问题很集中：

| 问题 | 表现 |
|---|---|
| 音源不稳定 | 公益接口限流严重，并发 2 个就返回 503 或空 URL |
| 版权缺失 | 网易云整批下架周杰伦等歌手，搜索层面就拿不到 id |
| 选曲不准 | 想要《东风破》，给你放 8 分钟的慢四版；想要《芒种》，给你放 91 秒小提琴版 |
| 试听截断 | 某些接口只返回 11.3 秒试听片段，播到一半就没了 |

改造后逐条解决：新增不依赖账号的酷我通道作为主力，用打分 + 时长中位数过滤解决选错版本，用多层回退解决单点失效。

---

## 音源链路

```
                    ┌─────────────────────────────────────────┐
  点歌请求           │  ① 酷我波点通道（主力，占比 >95%）        │
  song + singer  →  │     · 匿名接口，不需要登录/不需要 id      │
                    │     · 返回完整 320kbps，非试听片段        │
                    │     · 实测 0.5–1.4s                     │
                    └──────────────┬──────────────────────────┘
                                   │ 未命中 / 分数 < 40
                                   ▼
                    ┌─────────────────────────────────────────┐
                    │  ② 网易云 API + UNM 解灰（备用）          │
                    │     · 官方搜索结果，歌手信息最准          │
                    │     · unblock=true 可跨源补齐无版权歌曲   │
                    │     · 需要本地跑 NeteaseCloudMusicApi    │
                    └──────────────┬──────────────────────────┘
                                   │ 服务不可用
                                   ▼
                    ┌─────────────────────────────────────────┐
                    │  ③ GD Studio 公益 API（末位兜底）         │
                    │     · 限流严重，仅作最后手段              │
                    └─────────────────────────────────────────┘
```

代码位置：`helper.go` 的 `requestAndCacheMusic()`，以及 `yuafengfreeapi.go` 的 `getRemoteMusicURLOnly()`。

### ① 酷我波点通道 `kuwo.go`（新增）

不带账号、不依赖任何平台的歌曲 id，这是能覆盖周杰伦等「网易云下架歌手」的关键。

搜索：

```
http://search.kuwo.cn/r.s?stype=comprehensive&encoding=utf8&rformat=json
  &mobi=1&show_copyright_off=1&searchapi=6&all=<歌名 歌手>
```

解析 `content[].musicpage.abslist` 里的 `MUSICRID / SONGNAME / ARTIST / ALBUM / DURATION`。

取直链：

```
http://bd-api.kuwo.cn/api/play/music/v2/audioUrl?br=320kmp3&musicId=<RID>
```

接口要求 MD5 签名 + 固定的 `devid`，并且对**付费歌曲**必须先发一次「看广告解锁」请求，否则返回 `code=20018 没有解锁付费歌曲`。这些细节都在 `kwBodianURL()` 里处理了。

### ② 网易云 API `ncm.go`

配合 [NeteaseCloudMusicApiEnhanced](https://github.com/NeteaseCloudMusicApiEnhanced/api-enhanced) 使用，默认连 `http://127.0.0.1:3000`，可用环境变量覆盖：

```bash
NCM_API_BASE=http://192.168.1.100:3000
```

启动时建议开启解灰：

```bash
PORT=3000
ENABLE_GENERAL_UNBLOCK=true
ENABLE_FLAC=true
```

然后 `/cloudsearch` 搜歌、`/song/url/v1?id=...&level=exhigh&unblock=true` 取链。解灰会通过 UNM 从酷我/酷狗/QQ/咪咕找同名曲，官方 id `186016`（晴天）能拿到 269.7 秒 / 55MB 的完整 FLAC。

> ⚠️ 注意：网易云把周杰伦整批下架了，**搜索接口、专辑、歌单、歌手页都拿不到 id**，所以这条路救不了周杰伦。另外 artist id `122200643` 是蹭流量的假号，周杰伦真实 id 是 `6452`。

### ③ GD Studio `gdstudio.go`

```
https://music-api.gdstudio.xyz/api.php
  types=search&source=netease|joox&name=<歌名>
  types=url&source=<源>&id=<id>&br=320|128|740|999
```

并发 2 个就可能 503，已做降并发 + 短超时 + 空结果重试，只当最后兜底。

---

## 选曲算法（重点）

「搜到了」不等于「放对了」。同一首歌在音源里往往有几十个版本：原唱、翻唱、伴奏、消音、Live、DJ 版、抖音版、慢四版、小提琴版、CCTV 剪辑版…… 这套打分逻辑就是用来挑出原唱的。

### 打分流程

1. **歌名归一化** `gdNorm()`
   全角转半角、去标点、去括号内容，另外单独处理了几个坑：
   - `°C` 与「度」等价（否则《热爱105度的你》匹配不上）
   - 简繁异体统一（否则《Lemon》的「米津玄師」匹配不上「米津玄师」）

2. **歌手模糊匹配**
   允许字符重合度打分，周杰伦额外识别 `jay chou` / `jay` 等写法。

3. **版本惩罚**
   命中以下关键词直接重罚并标记 `fatal`，基本等于淘汰：

   ```
   翻唱 伴奏 消音 纯音乐 Live DJ 抖音版 慢四 超燃 小提琴 CCTV 电台
   ```

4. **时长中位数过滤**
   取所有候选（60–600 秒）时长的中位数，短于中位数 **55%** 的判为截断片段丢弃。
   这一步解决了两个真实案例：
   - 《海底》选中 80 秒截断版 → 现在能选到完整版
   - 《东风破》《烟花易冷》《兰亭序》选中 480 秒慢四版 → 现在能选到原版

5. **同分择优**
   分数相同时，优先「时长最接近中位数」的版本，避免超长串烧或加速版胜出。

6. **及格线下放**
   最终分数 **低于 40 分不强行播放**，直接交给下一层音源，宁可换源也不放错歌。
   （曾经因为「凑合放」，把《后来遇见他》放成了《大梦归》，这个阈值就是为它加的。）

### 实测效果

| 类别 | 通过率 |
|---|---|
| 周杰伦 30 首 | 30/30 原唱，晴天 269s、夜曲 226s、稻香 223s、七里香 299s、青花瓷 239s、以父之名 342s |
| 其他歌手 28 首（含粤语/日语/英文） | 28/28 正确 |

没有原唱版权时，按配置允许回退到翻唱版本（例如《热爱105度的你》最终走网易云翻唱）。

---

## 编译与运行

### 依赖

- Go 1.25+
- **ffmpeg**（必须，用于把音频转成设备需要的 24kHz 单声道 MP3）

```bash
# Windows 把 ffmpeg 加进 PATH，例如
set PATH=D:\ffmpeg-master-latest-win64-gpl\bin;%PATH%
```

### 编译

```bash
cd MeowMusicServer
go build -o MeowMusicServer.exe .
```

### 运行

```bash
./MeowMusicServer.exe
```

默认监听 `0.0.0.0:2233`，浏览器打开 `http://localhost:2233` 可以看到 Web 管理界面。

首次运行会在当前目录生成 `devices.json`，用于保存设备绑定关系。

### 可选：启用网易云通道

```bash
cd NeteaseCloudMusicApiEnhanced
npm install
PORT=3000 ENABLE_GENERAL_UNBLOCK=true ENABLE_FLAC=true node app.js
```

不启动也能正常工作，只是少了一层兜底。

---

## 主要接口

| 路径 | 说明 |
|---|---|
| `GET /stream_pcm?song=&singer=` | **核心接口**，返回设备可播的音频流 |
| `GET /stream_live?song=&singer=` | 流式转码直出 |
| `GET /api/search?keyword=` | 搜索歌曲 |
| `GET /api/favorite/list` | 收藏列表 |
| `GET /api/user/playlists` | 我的歌单 |
| `POST /api/esp32/bind` | 设备绑定 |
| `GET /api/esp32/verify` | 设备鉴权 |

完整路由见 `main.go` / `api.go` / `index.go`。

---

## 与 ESP32 固件对接

配套固件：[小智 AI 音乐固件（ESP32-S3）](https://github.com/joe12801/xiaozhi-esp32-music)，语音点歌后由固件发起：

```
GET http://<服务器IP>:2233/stream_pcm?song=晴天&singer=周杰伦
```

服务器返回 **24kHz / 单声道 / MP3**，固件边下边解码播放。

固件里只需把 `main/server_config.h` 的 `MUSIC_SERVER_URL` 改成这台服务器的局域网 IP。

---

## 目录结构

```
├── main.go              # 入口、路由注册
├── kuwo.go              # ★ 酷我波点通道（新增，主力音源）
├── ncm.go               # ★ 网易云 API 通道（改用 UNM 解灰）
├── gdstudio.go          # GD Studio 公益音源
├── yuafengfreeapi.go    # 原版多源适配（已接入新优先级）
├── helper.go            # ★ 音源调度 requestAndCacheMusic()、转码与缓存
├── device.go            # 设备绑定与 Token 管理
├── playlist.go          # 歌单与收藏
├── user.go              # 用户系统（bcrypt）
├── index.go             # Web 界面
├── file.go              # 文件与目录服务
├── frontend/ theme/     # 前端资源
├── files/               # 运行时数据（歌单、用户、背景图）
└── cache/               # 音频缓存
```

★ 标记的是本次改造涉及的核心文件。

---

## 已知限制

- **酷我接口属于非官方接口**，随时可能变更或失效，届时需更新签名参数与 `devid` 逻辑
- 酷我 `devid` 必须整个进程固定（解锁状态按设备绑定），重启后已解锁的付费曲需要重新解锁
- 网易云通道救不了被整批下架的歌手（如周杰伦），这是平台侧限制
- GD Studio 限流严重，不要指望它扛主流量
- 转码依赖 ffmpeg，没装会直接报错

## 免责声明

本项目仅供个人学习与技术研究使用，**不得用于商业用途**。所有音乐内容版权归各唱片公司及音乐平台所有。项目本身不存储任何音频文件，仅做实时转发与转码。

## 许可证

MIT License，详见 [LICENSE](LICENSE)。上游版权归 [OmniX-Space/MeowMusicServer](https://github.com/OmniX-Space/MeowMusicServer) 作者所有。
