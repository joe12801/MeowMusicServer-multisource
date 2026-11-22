#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 PWA 图标（纯标准库，无 PIL 依赖）。

画的是「双八分音符」：粉→紫渐变圆角底 + 两个倾斜符头 + 两根符干 + 顶部横梁。
输出：
  icon-192.png       常规（圆角，桌面/安卓列表用）
  icon-512.png       常规大图
  icon-maskable.png  全铺背景 + 内容缩到 80%，安卓自适应图标用
  apple-touch-icon.png 180x180 全铺背景（iOS 自己会切圆角）
"""
import math
import os
import struct
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT_DIR = os.path.join(ROOT, "theme", "icons")

# 主题粉 / 主题紫（与 full-app 的 --primary-color 系保持一致）
C1 = (0xFF, 0x8F, 0xB1)
C2 = (0x7C, 0x5C, 0xFF)


def lerp(a, b, t):
    return a + (b - a) * t


def mix(c1, c2, t):
    return tuple(int(round(lerp(c1[i], c2[i], t))) for i in range(3))


def rounded_rect_alpha(x, y, w, h, r):
    """点 (x,y) 在圆角矩形内的覆盖：返回 0..1（边缘 1px 抗锯齿）。"""
    cx = min(max(x, r), w - r)
    cy = min(max(y, r), h - r)
    d = math.hypot(x - cx, y - cy)
    # 距离圆角圆心 distance <= r 即在内部
    return clamp01(r - d + 0.5)


def clamp01(v):
    return 0.0 if v < 0 else (1.0 if v > 1 else v)


def in_ellipse(px, py, cx, cy, rx, ry, deg):
    """旋转椭圆内部判定。"""
    a = math.radians(deg)
    dx, dy = px - cx, py - cy
    u = dx * math.cos(a) + dy * math.sin(a)
    v = -dx * math.sin(a) + dy * math.cos(a)
    return (u / rx) ** 2 + (v / ry) ** 2 <= 1.0


def in_rect(px, py, x0, y0, x1, y1):
    return x0 <= px <= x1 and y0 <= py <= y1


def in_quad(px, py, pts):
    """凸四边形点内判定（叉积同号）。"""
    sign = 0
    for i in range(4):
        x1, y1 = pts[i]
        x2, y2 = pts[(i + 1) % 4]
        cross = (x2 - x1) * (py - y1) - (y2 - y1) * (px - x1)
        if cross == 0:
            continue
        s = 1 if cross > 0 else -1
        if sign == 0:
            sign = s
        elif s != sign:
            return False
    return True


def note_coverage(px, py, scale, ox, oy):
    """白色音符的覆盖度。坐标已归一化到 [0,1] 的艺术区（scale/offset 处理安全区）。"""
    # 先换算回未缩放的 0..1 空间
    nx = (px - ox) / scale
    ny = (py - oy) / scale
    if not (0 <= nx <= 1 and 0 <= ny <= 1):
        return 0.0

    # 两个符头（斜着的椭圆）
    if in_ellipse(nx, ny, 0.335, 0.700, 0.132, 0.101, -20):
        return 1.0
    if in_ellipse(nx, ny, 0.665, 0.620, 0.132, 0.101, -20):
        return 1.0

    # 两根符干
    if in_rect(nx, ny, 0.443, 0.285, 0.487, 0.700):
        return 1.0
    if in_rect(nx, ny, 0.773, 0.205, 0.817, 0.620):
        return 1.0

    # 顶部横梁（倾斜的四边形）
    if in_quad(nx, ny, [(0.443, 0.285), (0.817, 0.205),
                        (0.817, 0.298), (0.443, 0.378)]):
        return 1.0
    return 0.0


def render(size, corner_ratio=0.235, art_scale=1.0, art_offset=0.0,
           full_bleed=False, supersample=3):
    """返回 RGBA 像素行。supersample 用于抗锯齿。"""
    ss = supersample
    w = h = size
    rows = []
    art_w = 0.74 * art_scale            # 音符在画布中的横向占比
    ox = art_offset + (1.0 - art_scale) * 0.5 + (1.0 - 0.74) * art_scale * 0.5
    # 音符艺术区居中：艺术区宽 art_w，左边距 = (1-art_w)/2
    ox = (1.0 - art_w) / 2.0
    oy = ox  # 艺术区按正方形处理（音符本身在 y 上占 0.2~0.8）

    for y in range(h):
        row = bytearray()
        for x in range(w):
            # 超采样累加
            acc_r = acc_g = acc_b = acc_a = 0.0
            for sy in range(ss):
                for sx in range(ss):
                    px = (x + (sx + 0.5) / ss) / w
                    py = (y + (sy + 0.5) / ss) / h

                    # 背景：左上→右下 的线性渐变
                    t = clamp01((px * 0.55 + py * 0.45))
                    bg = mix(C1, C2, t)
                    # 左上角加一点柔光，避免纯色太平
                    glow = clamp01(1.0 - math.hypot(px - 0.18, py - 0.12) / 0.75) * 0.20
                    bg = tuple(min(255, int(bg[i] + 255 * glow * 0.6)) for i in range(3))

                    # 形状遮罩
                    if full_bleed:
                        shape_a = 1.0
                    else:
                        r = corner_ratio
                        shape_a = rounded_rect_alpha(px * w, py * h, w, h, r * w)

                    nc = note_coverage(px, py, art_w, ox, oy)
                    if nc > 0:
                        col = tuple(int(round(bg[i] + (255 - bg[i]) * nc)) for i in range(3))
                    else:
                        col = bg

                    acc_r += col[0]
                    acc_g += col[1]
                    acc_b += col[2]
                    acc_a += shape_a
            n = ss * ss
            rows.append((int(round(acc_r / n)), int(round(acc_g / n)),
                         int(round(acc_b / n)), int(round(255 * acc_a / n))))
        out = bytearray()
        for (r_, g_, b_, a_) in rows:
            out += bytes((r_, g_, b_, a_))
        rows = []
        yield bytes(out)


def write_png(path, size, **kw):
    raw = bytearray()
    for row in render(size, **kw):
        raw += b"\x00" + row  # filter type 0

    def chunk(tag, data):
        c = struct.pack(">I", len(data)) + tag + data
        return c + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)  # 8bit RGBA
    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) +
           chunk(b"IDAT", zlib.compress(bytes(raw), 9)) + chunk(b"IEND", b""))
    with open(path, "wb") as f:
        f.write(png)
    print(f"  {os.path.basename(path):24s} {size}x{size}  {len(png)/1024:.1f} KB")


def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    print("生成图标到", OUT_DIR)
    write_png(os.path.join(OUT_DIR, "icon-192.png"), 192)
    write_png(os.path.join(OUT_DIR, "icon-512.png"), 512)
    # 安卓 maskable：背景全铺，内容缩到 80% 安全区
    write_png(os.path.join(OUT_DIR, "icon-maskable-512.png"), 512,
              full_bleed=True, art_scale=0.80)
    # iOS 主屏图标：系统会自己切圆角，这里给全铺方图
    write_png(os.path.join(OUT_DIR, "apple-touch-icon.png"), 180,
              full_bleed=True, art_scale=0.86)
    # 浏览器标签页小图标
    write_png(os.path.join(OUT_DIR, "favicon-32.png"), 32, supersample=4)


if __name__ == "__main__":
    main()
