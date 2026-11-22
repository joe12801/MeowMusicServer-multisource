# 加 HTTPS：三种可行路径（按你的环境选一条）

现在服务是 `http://<SERVER_IP>:2233` 明文直连，能听歌，但有几件事做不了：

| 能力 | HTTP | HTTPS |
| --- | --- | --- |
| PWA 安装到手机主屏 | ✗（浏览器要求安全上下文） | ✓ |
| Service Worker 离线缓存 | ✗ | ✓ |
| 登录 Cookie 带 `Secure` | ✗（会被浏览器丢掉） | ✓ |
| 分享链接给别人不报警告 | ✗（Chrome 会标"不安全"） | ✓ |
| 手机 Safari / 微信内置浏览器播放 | 部分受限 | ✓ |

> **先看环境结论（已实测）**：这台 NAS 的 `80` / `443` 端口被飞牛的 nginx 占着
> （`ss -lntp` 显示 `nginx pid=3673`），所以 **Caddy / Nginx 容器抢不到 80/443**，
> Let's Encrypt 的 HTTP-01 验证也就没法直接做。下面三条路里，**A 最省事**。

---

## 路线 A：用飞牛 OS 自带的反向代理（推荐，零命令行）

前提：有一个域名（任何注册商都行，比如 `music.example.com`），并在 DNS 里加一条
A 记录指向 `<SERVER_IP>`。

1. 飞牛 OS → **设置 → 系统设置 → 端口与访问**（不同版本叫「远程访问 / 反向代理」），
   找到 **反向代理 / 域名访问** 功能；
2. 新增一条：
   - 域名：`music.example.com`
   - 目标：`http://127.0.0.1:2233`
   - 开启 **HTTPS**，证书选 **Let's Encrypt 自动申请**（飞牛会自己续期）；
3. 保存后访问 `https://music.example.com`，应该直接进播放器。

然后改 `.env` 让服务知道自己的对外地址（影响导出的 m3u 绝对地址、Cookie 的
`Secure` 标记判断）：

```bash
PUBLIC_BASE_URL=https://music.example.com
```

```bash
cd /vol1/docker/meowmusic && docker compose up -d --force-recreate
```

**反代必须满足的两点**（飞牛自带反代默认就满足，自建 nginx 要注意）：

```
proxy_set_header Host $host;                  # 导出 m3u / 重定向要用真实 Host
proxy_set_header X-Forwarded-Proto $scheme;   # 服务靠它判断是不是 HTTPS（Secure Cookie）
proxy_read_timeout 3600s;                     # 流式转码是长连接，别 60 秒就掐断
client_max_body_size 20m;                     # 上传封面/背景图
proxy_buffering off;                          # 边转边播，缓冲会让首播变慢
```

---

## 路线 B：有别的空闲端口 → Caddy 容器反代

如果 80/443 拿不到，但 `8443` 之类空着，可以让 Caddy 在 8443 上做 TLS：

```bash
cd /vol1/docker/meowmusic/deploy/https
cp .env.example .env      # 填 MEOW_DOMAIN 和 MEOW_HTTPS_PORT
docker compose -f docker-compose.caddy.yml up -d
```

- 优点：证书自动申请续期，一个文件搞定；
- 缺点：URL 带端口（`https://music.example.com:8443`），而且 **80 端口不在你手里时
  Let's Encrypt 的 HTTP-01 验不过**，只能走 DNS-01（需要把 DNS 服务商的 API Token
  填进 Caddyfile，见文件里的注释）。

---

## 路线 C：暂时不折腾 HTTPS

不想买域名、也不想动网络设置，那就继续用 `http://<SERVER_IP>:2233`。
除了 PWA 安装和离线缓存用不了，其余功能（多源播放、歌词、歌单、历史、
设备遥控、自动备份）都正常。

**这种情况下建议做两件事**：

1. 把 `.env` 里 `ADMIN_PAGE_TOKEN` 设成一个长随机串，这样即使登录出问题，
   还能用 `/status?key=口令` 进去看状态；
2. 别把地址发到公开群里 —— 现在有游客试听限制（默认 20 首/天/IP）兜底，
   但知道地址的人越多，转码额度消耗越快。

---

## 上线后自检（30 秒）

```bash
# 1. 反代有没有把协议头透传对（返回 https 才对）
curl -s https://music.example.com/api/guest/quota -o /dev/null -w '%{http_code}\n'

# 2. 登录后看 Cookie 是否带 Secure（浏览器 F12 → Application → Cookies）
#    没带 Secure 通常是反代漏了 X-Forwarded-Proto

# 3. PWA 是否可装：Chrome 地址栏右侧应出现"安装"图标
#    没有的话确认 /manifest.webmanifest 和 /sw.js 都是 200
curl -s -o /dev/null -w 'manifest=%{http_code} ' https://music.example.com/manifest.webmanifest
curl -s -o /dev/null -w 'sw=%{http_code}\n' https://music.example.com/sw.js
```

## 回滚

反代只是多一层转发，出问题直接删掉那条反代规则即可，服务本身还在 `:2233` 上跑，
数据（`/vol1/docker/meowmusic/data`）不受影响。
