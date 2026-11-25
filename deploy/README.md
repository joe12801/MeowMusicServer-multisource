# 一页版部署：把喵喵音乐服务跑在任意 Docker 主机上

适用于「别人给我一台服务器 / NAS，我要把服务装上去」的场景。
**不需要装 Go、不需要源码、不需要 ffmpeg**，只要 Docker，镜像里已带 ffmpeg。

- 镜像：`joe1280/meowmusic:latest`（备用：`ghcr.io/joe12801/meowmusic:latest`）
- 架构：`linux/amd64`、`linux/arm64` 双架构，x86 云服务器 / ARM NAS / 树莓派都是同一份镜像

---

## 0. 前置检查（10 秒）

```bash
docker -v                 # 需要 20.10+
docker compose version    # 需要 v2（v1 的 docker-compose 也能用，命令改成 docker-compose）
uname -m                  # x86_64 或 aarch64，两种都支持，无需自己选
```

没有 Docker 的话：

```bash
curl -fsSL https://get.docker.com | sudo sh          # 官方脚本，国内可换成阿里云镜像源
sudo usermod -aG docker $USER && newgrp docker       # 免 sudo 跑 docker
```

---

## 1. 方式 A：docker compose（推荐，后续升级最省事）

```bash
# 选一个数据盘上的目录，别放根分区（音频缓存会持续增长）
export MEOW_HOME=/opt/meowmusic
mkdir -p $MEOW_HOME && cd $MEOW_HOME

# 拿编排文件（也可以直接从仓库复制 deploy/docker-compose.yml 与 .env.example）
curl -fsSLO https://raw.githubusercontent.com/joe12801/MeowMusicServer-multisource/main/deploy/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/joe12801/MeowMusicServer-multisource/main/deploy/.env.example -o .env

# 生成 .env 并改成本机路径/端口
sed -i "s#^MEOW_DATA_DIR=.*#MEOW_DATA_DIR=$MEOW_HOME/data#" .env
sed -i "s#^MEOW_PORT=.*#MEOW_PORT=2233#" .env
[ -n "$MUSIC_MP3API_KEY" ] && sed -i "s#^MUSIC_MP3API_KEY=.*#MUSIC_MP3API_KEY=$MUSIC_MP3API_KEY#" .env

# 预建数据目录。注意 devices.json 必须是「文件」：
# 若不存在就让 docker 去挂，docker 会建成同名目录，服务将无法写入设备绑定表
mkdir -p data/files data/cache
[ -f data/devices.json ] || echo '{"devices":{}}' > data/devices.json

# 启动
docker compose pull && docker compose up -d
```

## 2. 方式 B：docker run（一条命令，不想装 compose 时用）

```bash
MEOW_HOME=/opt/meowmusic
mkdir -p $MEOW_HOME/data/files $MEOW_HOME/data/cache
[ -f $MEOW_HOME/data/devices.json ] || echo '{"devices":{}}' > $MEOW_HOME/data/devices.json

docker run -d --name meow-music --restart unless-stopped \
  -p 2233:2233 \
  -e TZ=Asia/Shanghai \
  -e PLAYLIST_SONGS_MAX=150 \
  -e MUSIC_MP3API_KEY=你的key \
  -v $MEOW_HOME/data/files:/app/files \
  -v $MEOW_HOME/data/cache:/app/cache \
  -v $MEOW_HOME/data/devices.json:/app/devices.json \
  joe1280/meowmusic:latest
```

升级（方式 B）：

```bash
docker rm -f meow-music   # 数据都在宿主机目录里，删容器不丢数据
# 然后重跑上面那条 docker run
```

---

## 3. 验证

```bash
docker compose ps                       # STATE 应为 running / healthy（方式 B：docker ps）
curl -I http://127.0.0.1:2233/          # 期望 HTTP/1.1 200
curl -s http://127.0.0.1:2233/api/search?keyword=晴天 | head -c 200
docker compose logs -f --tail=50        # 看启动日志
```

浏览器打开 `http://服务器IP:2233/` 能看到 Web 控制台就成功了。
接着在「设备绑定」页把 ESP32 音箱绑上，音箱端服务器地址填 `http://服务器IP:2233`。

> 端口要从外网/其他网段访问的话，记得放行防火墙：
> `sudo ufw allow 2233/tcp` 或 `sudo firewall-cmd --add-port=2233/tcp --permanent && sudo firewall-cmd --reload`，
> 云服务器还要在安全组里放行同一端口。

---

## 4. 配置项

写在 compose 同目录的 `.env` 里（方式 B 就是 `-e`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `MEOW_IMAGE` | `joe1280/meowmusic:latest` | 镜像地址，回滚可写 `:sha-<短提交>` |
| `MEOW_PORT` | `2233` | 宿主机对外端口 |
| `MEOW_DATA_DIR` | `./data` | 宿主机数据目录（歌单/用户/设备绑定/缓存） |
| `PLAYLIST_SONGS_MAX` | `150` | 设备端单次下发歌单上限。**不要超过 150**，ESP32 会因内存不足拉到歌单却不播 |
| `MUSIC_MP3API_KEY` | 空 | mp3api 外部音源密钥，播放国内下架歌曲需要；留空则跳过该源 |
| `MUSIC_SOURCES` | 全开 | 音源白名单，如 `kuwo,netease,qq,gdstudio`，排障用 |
| `MUSIC_SOURCE_MODE` | `auto` | 选曲模式 |
| `NCM_API_BASE` | 空 | 自建网易云 API 地址，配了才走该通道 |
| `WEBSITE_NAME_CN` 等 | — | 站点标题/描述/图标等展示项 |

改了 `.env` 之后：`docker compose up -d`（改 `PORT` 需要重建容器）。

---

## 4.5 汽水音乐增强（可选）

主服务已内置汽水音源筛选，但要真正搜全库/完整播放，还需 sidecar 与搜索签名绕过。
仓库提供了自包含的增强包 [`deploy/qishui/`](qishui/README.md)：

- `gomusic` sidecar（`guohuiyuan/go-music-api`）负责汽水取链 / 榜单 / 扫码登录；
- `qishui-proxy` 轻量代理把搜索改走免签名公开接口（官方接口已要求原生签名，未签名返回空）；
- 一键脚本 `deploy/qishui/upgrade.sh` 负责拉最新镜像并重建三个容器，密钥从 `.env` 读取，不写死。

快速上手：

```bash
cp deploy/qishui/proxy.cjs $MEOW_HOME/qishui-proxy/proxy.cjs
echo '{}' > $MEOW_HOME/data/qishui/cookies.json && chown 1000:1000 $MEOW_HOME/data/qishui/cookies.json
bash deploy/qishui/upgrade.sh
# 需要汽水全库搜索时，按 deploy/qishui/README.md 的扫码登录步骤写入 cookie
```

> 注意：出口 IP 需在境内；抖音域 cookie 对汽水无效且会触发风控。详见增强包 README。

---

## 5. 日常运维

```bash
docker compose logs -f --tail=100      # 日志
docker compose restart                 # 重启
docker compose pull && docker compose up -d    # 升级到最新
docker compose down                    # 停掉（数据保留）

# 备份（只需备份 data 目录）
tar czf meowmusic-data-$(date +%F).tar.gz -C $MEOW_HOME data

# 迁移到新机器：把 data 目录拷过去，改 .env 里的路径，重新 up -d 即可
```

回滚到某个历史版本：

```bash
sed -i 's#^MEOW_IMAGE=.*#MEOW_IMAGE=joe1280/meowmusic:sha-1a2b3c4#' .env
docker compose pull && docker compose up -d
```

### 播放预热（解决「首次播放要等十几秒」）

`/stream_live` 对未缓存的歌是**先整首转码落盘、再定长回包**，所以第一次听某首歌必然要等。
在 4 核小机器上实测（Beyond《农民》，10.4MB / 4 分钟）：

| 环节 | 耗时 |
|---|---|
| 从音源下载整首 | 2 秒 |
| ffmpeg 转 24kHz/32kbps 单声道 | **17 秒** |
| 命中缓存后再播放 | 0.05 秒 |

`-threads 1`、`-threads 0`、去掉 `nice -n 19`、CBR 换 VBR 都试过，差别只有 1~2 秒——
瓶颈就是 CPU 本身（如果同机还跑着 NVR 转码，会更慢）。所以别指望调参数，直接把歌提前转好：

```bash
# 脚本随 deploy 目录提供，拷到宿主即可
chmod +x warm-playlist.sh
./warm-playlist.sh          # 转全部歌单（已缓存的 0.1 秒跳过，幂等，可重复跑）
./warm-playlist.sh 20       # 只跑前 20 首，先看效果

# 后台跑，夜间补齐（歌单新增歌后再跑一次）
nohup setsid ./warm-playlist.sh > /tmp/warm.log 2>&1 < /dev/null &
tail -f /tmp/warm.log
```

脚本走 `127.0.0.1`，串行执行，任意时刻只占一个 ffmpeg，不会把正在播放的歌挤掉。
109 首的歌单里若有一半没缓存，补齐约需 15~20 分钟。

---

## 6. 排障

| 现象 | 处理 |
|---|---|
| `pull` 卡住 / `connection reset` | 换备用源：`MEOW_IMAGE=ghcr.io/joe12801/meowmusic:latest`；或先配好 Docker 代理/镜像加速再拉 |
| `no matching manifest` | 宿主是老 32 位 ARM（armv7），本镜像只有 amd64/arm64，换机器或自行构建 |
| 启动即退出，日志提示 devices.json 是目录 | 宿主上该路径被 docker 建成了目录：`rmdir data/devices.json && echo '{"devices":{}}' > data/devices.json`，再 `up -d` |
| 2233 被占用 | 改 `.env` 的 `MEOW_PORT`，容器内端口不用动 |
| 网页能开，音箱拉歌单无声音 | 先确认 `PLAYLIST_SONGS_MAX` ≤ 150；再看 `logs` 里有没有 `/stream_pcm` 请求进来（没有请求＝设备端拿不到列表或地址不对） |
| 播放国内下架歌失败 | 配 `MUSIC_MP3API_KEY`；首次播放需服务端转码，可能等 60~90 秒，属正常 |
| 有些歌要等十几秒才出声 | 未缓存，正在整首转码。用 `deploy/warm-playlist.sh` 提前转好（见第 5 节） |
| 日志里 `Cache miss` 远多于 `Serving from cache` | 同上，歌单里的歌大多还没转码；先跑一次预热 |
| 时间显示不对 | 确认 `TZ=Asia/Shanghai` 且宿主时间正常 |
| 磁盘被缓存吃满 | 缓存都在 `data/cache`，可定期清理旧文件；务必把 `MEOW_DATA_DIR` 放在大盘 |

---

## 7. 关于自动部署

仓库的 `.github/workflows/docker-build-deploy.yml` 负责构建多架构镜像并推送到 Docker Hub / GHCR / 阿里云 ACR，
`main` 一 push 就跑，**但不管部署** —— 部署是独立的手动 workflow `.github/workflows/deploy-nas.yml`。

其中 `deploy-nas.yml` 绑定的是**维护者 NAS 上的自托管 runner**（标签 `self-hosted, nas`），
在别人的服务器上不会生效，也不需要配 —— 手动 `docker compose pull && up -d` 就行。
这样设计是因为部署目标可能关机：早先把部署塞在构建 workflow 里，NAS 离线时 queued 的 job
会占住并发组，把后续所有构建都卡成 pending。

想给自己的机器也接入自动部署，做法是：在该机器装一个 GitHub Actions runner 并打上 `nas` 标签，
然后手动触发 `Deploy to NAS`；同时确保仓库 Secrets 里配了 `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN`
（仅构建需要）。
