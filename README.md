# MeowMusicServer · 多音源增强版

给嵌入式设备（ESP32 等）用的局域网音乐流媒体服务器。

**核心改造：把原来单一的音源换成了「酷我 → 网易云直连 → 小枫 xfabe → QQ 音乐 → 酷狗 → 网易云本地 API → 酷我宽容模式 → GD Studio → yuafeng」九层自动回退链路，并加了一套选曲打分算法，解决点歌时最常见的两个问题——「搜不到」和「放错版本」。**

实测 35 首中文/粤语/日语歌曲（热门 + 冷门独立音乐混合），全部命中正确歌手与正确时长，首字节耗时 **0.5–1.4 秒**。

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

同一条链路上跑九个源，前五个是「严格模式」（只要原唱），后面才是宽容兜底。

```
  点歌请求（song + singer）
        │
        ▼
 ┌───────────────────────────────────────────┐
 │ ① 酷我（严格）      主力，实测占比 ~90%     │
 │   付费大牌唯一可用通道，完整 320k           │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ② 网易云直连        有版权且免 VIP 的曲目   │
 │   不依赖本地 node 服务，0.6s 出链           │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ③ 小枫 xfabe        代取网易云 VIP 直链      │
 │   撞 405 自动静默 15 分钟（当前即此状态）     │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ④ QQ 音乐           免费曲池                 │
 │   独立 / 民谣 / 网络歌曲覆盖最好（冷门 6/10）│
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ⑤ 酷狗              免费曲补充               │
 │   曲库最厚但付费比例高（冷门 1/10）          │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ⑥ 网易云本地 API（可选）带 UNM 解灰          │
 │   同机跑了 ncm 服务才有用，否则瞬间返回       │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ⑦ 酷我（宽容）      Live / 翻唱版顶上        │
 │   上面全空时才用，能听比没歌强               │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ⑧ GD Studio         公益聚合源               │
 │   境外部署时唯一能用的（境内四家都限地域）    │
 └───────────────────┬───────────────────────┘
                     ▼
 ┌───────────────────────────────────────────┐
 │ ⑨ yuafeng           最后兜底                 │
 └───────────────────────────────────────────┘
```

代码位置：`helper.go` 的 `requestAndCacheMusic()`。所有走「实时流式播放」的请求（`/stream_live`）也走同一条链——见下面这条坑。

> **坑：两条链路曾经是分开的。**
> 原版 `yuafengfreeapi.go` 里的 `getRemoteMusicURLOnly()` 自己硬编码了一套「酷我 → 网易云 → GDStudio → yuafeng」顺序，而 `requestAndCacheMusic()` 是另一套。设备实际播放走的是 `/stream_live` → `getRemoteMusicURLOnly()`，所以新增音源后**只有网页端 `/stream_pcm` 生效，设备完全用不上**，日志里看不到任何新源动静。现在两个入口统一到 `requestAndCacheMusic()`，音源顺序永远一致。

### ① 酷我波点通道 `kuwo.go`（主力）

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

> **性能坑：解锁请求必须只发一次。**
> 早期实现是「每取一条链接就解锁一次」，等于每次点歌多花 3~5 秒。实测同一批歌里，《海阔天空》《起风了》《生僻字》首字节 5.8~6.3 秒，而刚好没触发重解的只要 0.9 秒。改成 `sync.Once` 后全部降到 0.5~1.4 秒；只有服务端返回 `20018` 时才强制重解一次（60 秒内不重复打，避免风控）。

### ② 网易云直连 `netease_direct.go`（新增）

**不依赖本地 ncm API 服务**，直接打 `music.163.com`。原来那条链路要求同机跑一个 node 版 API，而 NAS / 境外小机器上根本没装 node，等于永久失效。

```
搜索  https://music.163.com/api/search/get/web?s=<歌名 歌手>&type=1&limit=15
取链  https://music.163.com/song/media/outer/url?id=<id>.mp3
```

关键点：**必须先 GET 一次首页把 cookie（NMTID 等）灌进 cookie jar**，否则取链接口会返回一个 107KB 的 HTML「暂无版权」页面，而不是音频。所以用 `http.Client{Jar: cookiejar}`，并在判断成功时校验 `Content-Type` 是否 `audio/*`——只看 HTTP 200 会把 HTML 丢给 ffmpeg，设备侧表现为「一直转圈然后静音」。

跳过 `fee==1`（VIP 专享）和 `fee==4`（付费专辑）的候选，它们的 outer url 必空。`fee==8`（下架但有低音质）反而经常能取到，不要一起跳过。

### ③ 小枫 xfabe `xfabe.go`（新增，实验性）

文档：<https://api.xfabe.com/info/wy_MuiscInfo/doc>（站点是 SPA，文档数据走
`POST https://api.xfabe.com/api/apiInfo/doc`，传 `{"docName":"wy_MuiscInfo"}`）

```
搜索  https://node.api.xfabe.com/api/wangyi/search?search=<歌名>&limit=15
取链  https://node.api.xfabe.com/api/wangyi/music?type=json&id=<网易云 id>
```

和「网易云直连」是同一个曲库，差别在取链：我们自己拼 `outer/url` 只能拿免 VIP 的，
这个接口由第三方后端代取，文档标注支持 VIP，返回带鉴权参数的 CDN 直链。

> ⚠️ **当前状态：搜索可用，取链被限流。**
> `/music` 对任何 id、任何 UA、任何 Referer、任何 IP（本机广州移动 / 境外马来西亚 VPS
> 都是第一发就中）统一返回 `{"code":405,"msg":"操作频繁，请稍候再试"}`，
> 而同一台服务器上的 `/search` 和 `/randomMusic` 都正常返回。
> 判断是服务端全局限流（可能是它自己上游的额度用完了），不是我们的参数或网络问题。
> 代码里做成了「撞到 405 立刻静默 15 分钟」，所以它不会拖慢后续音源；
> 等对方恢复，这条通道会自动开始出货，不需要改代码。

其它坑：

- `duration` 单位是**毫秒**，不是秒。
- 字段名是 `artistsname`（少一个 t）。
- 搜索结果里翻唱占大头（搜「晴天」第一条是「晴天 (原唱 周杰伦) - RyaVocal」），
  所以照样要过 `srcNoiseOf` / `srcTitleScore` / `kwFatalArtistMarkers` 三套闸门；
  实测 `海底` 能选出 `一支榴莲 256s`（score=190）这一条正确候选。

### ④ QQ 音乐 `qqmusic.go`（新增）

```
搜索  https://c.y.qq.com/soso/fcgi-bin/client_search_cp?new_json=1&cr=1&p=1&n=15&w=<歌名 歌手>&format=json
取链  https://u.y.qq.com/cgi-bin/musicu.fcg   (module=vkey.GetVkeyServer, method=CgiGetVkey)
```

无登录态也能拿 vkey，但**只有 `pay.pay_play==0` 的免费曲 `purl` 才非空**。周杰伦这类数字专辑（`pay_play==1`）返空，本源无能为力。反过来，它在独立/民谣/网络歌曲上覆盖极好——冷门曲目实测 6/10，比酷狗强得多，所以排在酷狗之前。

免费曲能拿到完整 m4a（实测 2.6MB / 213 秒 / 98kbps），ffmpeg 可直接读。

> **坑：参数不能"补全"。**
> 带上官方网页那套 `remoteplace=txt.yqq.song` / `searchid=0` 之后，接口对任何关键词都稳定返回 `curnum:0`（0 条）。去掉这两个参数，同样的关键词立刻返回 15 条。别照着网页请求抄参数。

### ⑤ 酷狗 `kugou.go`（新增）

```
搜索  http://mobilecdn.kugou.com/api/v3/search/song?format=json&keyword=<歌名 歌手>&page=1&pagesize=15
取链  http://m.kugou.com/app/i/getSongInfo.php?cmd=playInfo&hash=<hash>
```

曲库最厚，但 `pay_type==3` 的付费曲取链必然返回 `The Resource Needs to be Paid`，所以只在免费曲里挑。

两个网络细节都是被实际环境逼出来的：

1. **必须走 http 且强制 IPv4。** `mobilecdn.kugou.com` 在部分机器上只解析出 IPv6，而该机器的 IPv6 出口是坏的——https 握手直接超时（curl 返回 `000`），http 走 IPv6 却能通。
2. **不能跟随重定向。** Go 默认会把 `http://m.kugou.com/...` 301 跳到 https，然后卡死在坏掉的 https 上，表现是「搜索有结果但取链永远失败」。`curl` 不加 `-L` 能直接拿到 JSON，代码里也照做（`CheckRedirect` 返回 `ErrUseLastResponse`）。

### ⑥ 网易云本地 API `ncm.go`（可选）

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

> ⚠️ 没装这个服务也完全不影响使用——`NCMSearch()` 会立刻 `connection refused` 返回空，链路上继续往下走。网易云把周杰伦整批下架了，**搜索接口、专辑、歌单、歌手页都拿不到 id**，所以这条路救不了周杰伦。另外 artist id `122200643` 是蹭流量的假号，周杰伦真实 id 是 `6452`。

### ⑦ 酷我宽容模式

同一个 `kuwo.go`，用 `KuwoSearch()` 而不是 `KuwoSearchStrict()`。差别只有一处：允许「有人声的 Live / 翻唱 / DJ 版」进入候选（纯器乐仍然永远排除）。

为什么需要它，又为什么必须排在后面：

- 排前面会出真问题。《理想三旬》− 陈鸿宇，酷我只收了两条：一条是 130 秒的「陈鸿宇&翻唱者：王煜航」拼接版，一条是 242 秒的 Live 版。原版 210 秒在 QQ / 网易云那儿。如果一上来就用宽容模式，会直接放半首翻唱。
- 排后面又必须留。上面七个源全空时，有一首 Live 版总比设备干等着没声强。

### ⑧ GD Studio `gdstudio.go`

```
https://music-api.gdstudio.xyz/api.php
  types=search&source=netease|joox&name=<歌名>
  types=url&source=<源>&id=<id>&br=320|128|740|999
```

并发 2 个就可能 503，已做降并发 + 短超时 + 空结果重试 + 音源冷却（连续失败 2 次冷却 15 分钟），只当最后兜底。

**境外部署时它是唯一能用的**：酷我返回 `code=407 仅限中国大陆地区使用`，酷狗 / QQ 同样限地域。代码里对酷我做了地域判定，检测到 407/433 就在本进程内停用，不再每次白等 5~10 秒。

### ⑨ yuafeng

`api.yuafeng.cn` / `api-v2.yuafeng.cn` 的免费接口，限流严重，只作最后手段。

---

## 版本噪声统一判定（`srcNoiseOf` / `srcTitleScore`）

新增的三个源（网易云直连 / QQ / 酷狗）有一个共同困境：**它们的免费曲池里，"二次加工版"占了大多数**。如果不做严格判定，会出现这些真实错误：

| 点了这首歌 | 实际放出来 | 问题 |
|---|---|---|
| 成都 / 赵雷 | 成都 (2026没有信号巡回演唱会北京站) | 演唱会现场版 |
| 大鱼 / 周深 | 大鱼 (3D环绕版) | 改版 |
| 山海 / 草东没有派对 | 山海 (DJ LayE版) | DJ 混音 |
| 麒麟 / 早安 | 《麒麟》我自有一口气 一剑开天门 | 102 秒短视频切片 |
| 理想三旬 / 陈鸿宇 | 理想三旬 (Live) 242s | 原版 210s 在别处 |

这些标题里**歌名和歌手都完全对得上**，只靠「歌名归一化 + 歌手匹配」打分会全部判成原唱。所以 `helper.go` 里加了两道公共闸门，三个源共用：

**第一道：关键词黑名单**（`srcNoiseWords`）
现场 / 演唱会 / 伴奏 / 消音 / 改编 / DJ / 抖音 / 慢四 / 3D / 环绕 / 变声 / CCTV / 综艺 … 命中即 `fatal`。

**第二道：括号后缀白名单**（`srcBracketSuffix` + `srcVersionWhitelist`）
标题括号里写了别的东西，基本就是在告诉你「这不是原版」。只有 `原版 / 正式版 / 单曲 / Single / Album Version / 完整版` 这类纯标注性后缀才放行（《海底 (Single Version)》确实是原曲）。用户自己搜的歌名里就带这个后缀时也放行。

> 注意括号必须从**原始标题**里取：`gdNorm()` 归一化会把括号连同内容整段删掉，等于把最重要的版本信息丢了。

**第三道：标题长度闸门**（`srcTitleScore`）
「包含匹配」不能直接给高分。QQ / 酷狗上大量结果是**视频标题**，把歌名当引子后面挂一长串描述。规则：标题比歌名长出一大截（超过 1.6 倍）就不认——`《麒麟》我自有一口气 一剑开天门`（17 字）对 `麒麟`（2 字）直接被判 `fatal`。

**第四道：歌手字段里的翻唱标记**（`kwFatalArtistMarkers`）
酷我有条目把歌手写成「陈鸿宇&翻唱者：王煜航」。歌手字段「包含陈鸿宇」所以歌手匹配通过、得分 160 排第一，但它是 130 秒的翻唱拼接版。命中 `翻唱 / cover / karaoke / dj / remix / 改编` 一律罚到阈值以下，连宽容模式的第二轮都不给机会。

> 顺带一条设计取舍：前四个源**不做**「没有原唱就退而求其次用现场版」的兜底。宁可这里返回空，让后面的源去试真原唱；所有源都没有时，⑥⑦⑧ 那三层保留了宽容策略，不至于断歌。

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

在 NAS（ARM64 / 2GB 内存，出口广东移动）上实测，共 35 首：

| 类别 | 通过率 | 备注 |
|---|---|---|
| 热门华语 / 粤语 / 日语 | 25 / 25 | 晴天 269s、稻香 223s、夜曲 226s、七里香 299s、海阔天空 324s、Lemon 256s |
| 冷门 / 独立 / 民谣 | 10 / 10 | 漠河舞厅 334s、理想三旬 209s、云烟成雨 240s、盗将行 198s、七月上 179s |
| **合计** | **35 / 35** | 首字节 0.5–1.4 秒，命中分布：酷我 32、网易云 3 |

各源单独隔离测试的命中率（同一批冷门曲目）：

| 音源 | 冷门命中 | 说明 |
|---|---|---|
| 酷我 | 10/10 | 主力，付费曲也能拿 |
| 网易云直连 | 5/8 | 只覆盖免 VIP 的 |
| QQ 音乐 | 6/10 | 免费曲池对独立音乐覆盖最好 |
| 酷狗 | 1/10 | 冷门曲几乎都要付费 |

没有原唱版权时，按配置允许回退到翻唱/Live 版本（⑦ 酷我宽容模式与 ⑦⑧）。

---

## 音源开关（排障用）

默认八个源全开。想定位「到底是哪个源在出货」或临时关掉某个不稳定的源，用环境变量收窄：

```bash
# 只开酷我
MUSIC_SOURCES=kuwo ./MeowMusicServer

# 只开网易云直连 + QQ
MUSIC_SOURCES=netease,qq ./MeowMusicServer

# 部署在境外（酷我/酷狗/QQ 全部限地域），直接只留公益源
MUSIC_SOURCES=gdstudio,yuafeng ./MeowMusicServer
```

可用值：`kuwo` `netease` `xfabe` `qq` `kugou` `gdstudio` `yuafeng`。（`netease` 同时控制「直连」和「本地 API」两条网易云链路。）

日志里每个请求都会先打一行当前开关状态，便于确认配置生效：

```
[Source] "晴天" - "周杰伦" | kuwo=true kugou=true netease=true qq=true gdstudio=true
[Kuwo] cand #1 score=170 fatal=false "晴天" - "周杰伦" (叶惠美, 269s)
[Kuwo] HIT #1 "晴天" - "周杰伦" (269s, score=170)
```

`MUSIC_SOURCE_MODE=overseas` 是另一层开关：启动时直接跳过酷我，避免在境外每首歌都白等一次 407。

---

## 部署

两个平台都只需要两样东西：**Go 1.25+** 和 **ffmpeg**。下面 Windows 和 Linux 各给一套完整步骤，任选其一。

### 通用：获取代码

```bash
git clone https://github.com/joe12801/MeowMusicServer-multisource.git
cd MeowMusicServer-multisource
```

---

### Windows 部署

#### 1. 安装 Go

**方式 A：winget（推荐，Win10 1809+）**

```powershell
winget install GoLang.Go
```

**方式 B：安装包**

打开 https://go.dev/dl/ 下载 `go1.25.x.windows-amd64.msi`，双击安装（默认装到 `C:\Program Files\Go`）。

装完**必须新开一个终端**，然后验证：

```powershell
go version
# 期望输出：go version go1.25.x windows/amd64
```

#### 2. 安装 ffmpeg

**方式 A：winget**

```powershell
winget install Gyan.FFmpeg
```

**方式 B：手动解压**

1. 下载 https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip
2. 解压到 `C:\ffmpeg`（确认 `C:\ffmpeg\bin\ffmpeg.exe` 存在）
3. 加进系统 PATH（**管理员** PowerShell 执行一次）：

```powershell
[Environment]::SetEnvironmentVariable(
  "Path",
  [Environment]::GetEnvironmentVariable("Path", "Machine") + ";C:\ffmpeg\bin",
  "Machine"
)
```

新开终端验证：

```powershell
ffmpeg -version
```

#### 3. 编译

```powershell
cd MeowMusicServer-multisource

# 国内网络建议先设代理，否则拉依赖会很慢
go env -w GOPROXY=https://goproxy.cn,direct

go mod tidy
go build -o MeowMusicServer.exe .
```

看到目录里出现 `MeowMusicServer.exe`（约 12MB）就成功了。

> 也可以直接双击 `start.bat`，它会自动检查 Go、拉依赖并 `go run .` 启动，适合先跑起来看看。

#### 4. 运行

**前置条件**：`ffmpeg` 必须在 PATH 里，否则点歌时会报 `failed to start ffmpeg`。

```powershell
# 默认 2233 端口
.\MeowMusicServer.exe

# 想换端口（临时）
$env:PORT = "8080"; .\MeowMusicServer.exe
```

或者在项目根目录建一个 `.env` 文件（长期生效）：

```ini
PORT=2233
```

启动后浏览器打开 http://localhost:2233 ，第一次访问会提示注册账户。

#### 5. 放行防火墙（关键）

不放行的话，ESP32 能连上 WiFi 但点歌一定超时。**管理员** PowerShell 执行：

```powershell
netsh advfirewall firewall add rule name="MeowMusic 2233" dir=in action=allow protocol=TCP localport=2233
```

如果用的是第三方安全软件（360、火绒等），还要在它的「网络防护」里给 `MeowMusicServer.exe` 放行。

#### 6. 查本机 IP 并填进固件

```powershell
ipconfig
```

找到当前 WiFi 网卡的 **IPv4 地址**（例如 `192.168.1.100`），填到固件的 `main/server_config.h`：

```c
#define MUSIC_SERVER_URL "http://192.168.1.100:2233"
```

#### 7. 开机自启（两种方式）

**方式 A：任务计划程序（简单）**

先建一个 `C:\meow\run.bat`，内容**必须**先 `cd` 到程序目录，否则 `devices.json` 会生成到 `C:\Windows\System32`：

```bat
@echo off
cd /d C:\meow
C:\meow\MeowMusicServer.exe
```

然后**管理员** CMD 注册开机任务：

```cmd
schtasks /create /tn "MeowMusicServer" /tr "C:\meow\run.bat" /sc onstart /ru SYSTEM /rl highest /f
```

管理：

```cmd
schtasks /run    /tn "MeowMusicServer"    :: 立即启动
schtasks /end    /tn "MeowMusicServer"    :: 停止
schtasks /delete /tn "MeowMusicServer" /f :: 删除
```

**方式 B：NSSM 注册成 Windows 服务（推荐，能设工作目录 + 崩溃自动重启）**

1. 下载 https://nssm.cc/download ，解压出 `nssm.exe`（放到 `C:\meow\`）
2. **管理员** CMD 执行：

```cmd
C:\meow\nssm.exe install MeowMusic "C:\meow\MeowMusicServer.exe"
C:\meow\nssm.exe set MeowMusic AppDirectory "C:\meow"
C:\meow\nssm.exe set MeowMusic AppEnvironmentExtra PORT=2233
C:\meow\nssm.exe set MeowMusic Start SERVICE_AUTO_START
C:\meow\nssm.exe start MeowMusic
```

之后在「服务」里就能看到 `MeowMusic`，改配置用 `nssm edit MeowMusic`。

---

### Linux 部署

以 Ubuntu 22.04 / Debian 12 为例，其他发行版对应替换包管理器即可。

#### 1. 安装 Go

发行版自带的 Go 版本往往偏低（`go.mod` 要求 1.25），建议用官方二进制：

```bash
# 版本号去 https://go.dev/dl/ 查最新的 1.25.x
wget https://go.dev/dl/go1.25.4.linux-amd64.tar.gz

sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.25.4.linux-amd64.tar.gz

# 写进 PATH（当前用户）
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile
source ~/.profile

go version
```

> ARM 服务器（树莓派、甲骨文 ARM 实例）把文件名换成 `go1.25.4.linux-arm64.tar.gz`。

#### 2. 安装 ffmpeg

```bash
# Debian / Ubuntu
sudo apt update && sudo apt install -y ffmpeg

# Fedora / RHEL（需先启用 RPM Fusion）
sudo dnf install -y ffmpeg

# Arch / Manjaro
sudo pacman -S --noconfirm ffmpeg
```

验证：

```bash
which ffmpeg && ffmpeg -version | head -1
```

> 记下 `which ffmpeg` 的路径（通常是 `/usr/bin/ffmpeg`），后面 systemd 里要确保它在 PATH 中。

#### 3. 编译

```bash
cd ~/MeowMusicServer-multisource

export GOPROXY=https://goproxy.cn,direct
go mod tidy
go build -o MeowMusicServer .
```

#### 4. 调整目录权限并试运行

```bash
# 建一个专用系统用户，避免用 root 跑
sudo useradd -r -s /usr/sbin/nologin meowmusic 2>/dev/null || true

sudo mkdir -p /opt/meow-music
sudo cp -r ./* /opt/meow-music/
sudo chown -R meowmusic:meowmusic /opt/meow-music

# 试跑（前台，Ctrl+C 停止）
cd /opt/meow-music
PORT=2233 ./MeowMusicServer
```

另开一个终端验证：

```bash
curl -s "http://127.0.0.1:2233/api/search?keyword=晴天" | head -c 200
```

#### 5. 注册成 systemd 服务（开机自启 + 崩溃自动重启）

新建 `/etc/systemd/system/meow-music.service`：

```ini
[Unit]
Description=Meow Music Server (多音源)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=meowmusic
Group=meowmusic
WorkingDirectory=/opt/meow-music
Environment=PORT=2233
# 如果启用了网易云通道，取消下面这行注释
# Environment=NCM_API_BASE=http://127.0.0.1:3000
ExecStart=/opt/meow-music/MeowMusicServer
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

启用：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now meow-music

sudo systemctl status meow-music       # 看运行状态
journalctl -u meow-music -f            # 实时看日志
```

常用管理：

```bash
sudo systemctl restart meow-music   # 改完配置重启
sudo systemctl stop meow-music      # 停止
sudo systemctl disable meow-music   # 取消开机自启
```

> **踩坑提醒**：systemd 的默认 PATH 是 `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`。
> 如果你的 ffmpeg 不在这些目录（例如自己编译到 `/opt/ffmpeg/bin`），必须在 unit 里补一行：
> `Environment=PATH=/opt/ffmpeg/bin:/usr/local/bin:/usr/bin:/bin`，否则服务能起但一点歌就失败。

#### 6. 放行防火墙

```bash
# ufw（Ubuntu/Debian）
sudo ufw allow 2233/tcp
sudo ufw reload

# firewalld（CentOS/RHEL/Fedora）
sudo firewall-cmd --permanent --add-port=2233/tcp
sudo firewall-cmd --reload
```

云服务器（阿里云/腾讯云/Oracle 等）还要在**控制台安全组**里额外放行 2233/TCP，这一步最容易漏。

#### 7. 查本机 IP

```bash
ip -4 addr show | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | grep -v 127.0.0.1
```

把结果填进固件的 `main/server_config.h`。**注意**：ESP32 和服务器必须在同一局域网；如果是云服务器，需要公网 IP 或内网穿透。

---

### 可选：启用网易云通道（两个平台通用）

这一步不是必需的，只是让「网易云原唱 + UNM 解灰」这层兜底生效。需要 Node.js 18+。

先把 API 项目拉到主服务器同级的目录（只做一次）：

```bash
git clone https://github.com/NeteaseCloudMusicApiEnhanced/api-enhanced.git
```

**Windows：**

```powershell
cd api-enhanced
npm install
$env:PORT="3000"
$env:ENABLE_GENERAL_UNBLOCK="true"
node app.js
```

**Linux：**

```bash
sudo apt install -y nodejs npm       # 或用 nvm 装 Node 20
cd api-enhanced
npm install
PORT=3000 ENABLE_GENERAL_UNBLOCK=true node app.js
```

启动后主服务器会自动连 `http://127.0.0.1:3000`；如果它跑在别的机器/端口，用环境变量指过去：

```bash
NCM_API_BASE=http://192.168.1.50:3000
```

Linux 下同样可以给它写一个 systemd unit（把 `ExecStart` 换成 `node app.js`，`WorkingDirectory` 指向 `api-enhanced`）。

---

### 可选：Docker 部署

不想装 Go 和 ffmpeg 环境的话，用容器最省事。项目根目录建 `Dockerfile`：

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
RUN go env -w GOPROXY=https://goproxy.cn,direct && \
    go mod tidy && \
    CGO_ENABLED=0 go build -o MeowMusicServer .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
WORKDIR /app
COPY --from=build /src/MeowMusicServer .
COPY --from=build /src/files ./files
COPY --from=build /src/frontend ./frontend
COPY --from=build /src/theme ./theme
ENV PORT=2233
EXPOSE 2233
CMD ["./MeowMusicServer"]
```

`docker-compose.yml`：

```yaml
services:
  meow-music:
    build: .
    container_name: meow-music
    ports:
      - "2233:2233"
    environment:
      - PORT=2233
      - TZ=Asia/Shanghai
    volumes:
      - ./cache:/app/cache          # 音频缓存
      - ./files:/app/files          # 歌单、用户数据、背景图
      - ./devices.json:/app/devices.json   # 映射前先建好：echo '{"devices":{}}' > devices.json
    restart: unless-stopped
```

```bash
docker compose up -d --build
docker compose logs -f
```

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
├── kuwo.go              # ★ 酷我波点通道（主力音源，含严格/宽容两档）
├── netease_direct.go    # ★ 网易云直连（新增，不依赖本地 API 服务）
├── xfabe.go             # ★ 小枫 xfabe（新增，代取 VIP 直链，限流自愈）
├── qqmusic.go           # ★ QQ 音乐通道（新增，免费曲）
├── kugou.go             # ★ 酷狗通道（新增，免费曲补充）
├── ncm.go               # ★ 网易云本地 API 通道（可选，带 UNM 解灰）
├── gdstudio.go          # GD Studio 公益音源（境外主力）
├── yuafengfreeapi.go    # 原版多源适配（已统一入口）
├── helper.go            # ★ 音源调度 requestAndCacheMusic()、版本噪声判定、转码与缓存
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
- 网易云直连依赖 `music.163.com` 首页下发的 cookie，接口收紧后需要重新调整
- QQ 音乐 / 酷狗只能取免费曲，付费曲（周杰伦的数字专辑等）依赖酷我
- 酷狗接口在只解析 IPv6 的机器上必须走 http + 强制 IPv4，否则 https 握手直接失败

## 免责声明

本项目仅供个人学习与技术研究使用，**不得用于商业用途**。所有音乐内容版权归各唱片公司及音乐平台所有。项目本身不存储任何音频文件，仅做实时转发与转码。

## 许可证

MIT License，详见 [LICENSE](LICENSE)。上游版权归 [OmniX-Space/MeowMusicServer](https://github.com/OmniX-Space/MeowMusicServer) 作者所有。
