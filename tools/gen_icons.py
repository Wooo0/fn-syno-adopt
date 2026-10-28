#!/usr/bin/env python3
"""生成应用图标 ICON.PNG (64x64) 与 ICON_256.PNG (256x256)。

设计：蓝渐变圆角方底 + 白色硬盘 + 琥珀色迁移环箭头（硬盘 + 环形箭头 = 磁盘迁移）。
纯 stdlib（zlib + struct），4x 超采样抗锯齿后降采样。
"""
import math
import struct
import zlib
from pathlib import Path

SIZE = 256
SS = 4  # 超采样倍数

# 品牌色
GRAD_TL = (59, 130, 246)   # #3B82F6
GRAD_BR = (30, 64, 175)    # #1E40AF
WHITE = (255, 255, 255)
AMBER = (252, 177, 4)      # #FCB104
SLOT = (37, 99, 235)       # #2563EB（硬盘槽线）


def lerp(a, b, t):
    return tuple(round(a[i] + (b[i] - a[i]) * t) for i in range(3))


def in_rounded_rect(x, y, x0, y0, x1, y1, r):
    if not (x0 <= x <= x1 and y0 <= y <= y1):
        return False
    cx = min(max(x, x0 + r), x1 - r)
    cy = min(max(y, y0 + r), y1 - r)
    return (x - cx) ** 2 + (y - cy) ** 2 <= r * r or (x0 + r <= x <= x1 - r) or (y0 + r <= y <= y1 - r)


def polar(cx, cy, r, deg):
    a = math.radians(deg)
    return cx + r * math.cos(a), cy + r * math.sin(a)


def in_triangle(px_, py_, a, b, c):
    # 标准叉积同侧测试：点 p 与三角形三边的有向面积同号则在内部
    def cross(ox, oy, ax, ay, bx, by):
        return (ax - ox) * (by - oy) - (ay - oy) * (bx - ox)
    d1 = cross(px_, py_, a[0], a[1], b[0], b[1])
    d2 = cross(px_, py_, b[0], b[1], c[0], c[1])
    d3 = cross(px_, py_, c[0], c[1], a[0], a[1])
    has_neg = d1 < 0 or d2 < 0 or d3 < 0
    has_pos = d1 > 0 or d2 > 0 or d3 > 0
    return not (has_neg and has_pos)


def render(w: int):
    """返回 w x w 像素矩阵（RGBA 元组），坐标系按 SIZE 设计等比缩放。

    圆角外 alpha=0（全透明），避免桌面壁纸下四角出现黑边。
    """
    s = w / SIZE
    CX, CY = 128, 132  # 环中心
    px = [[(0, 0, 0, 0)] * w for _ in range(w)]
    # 箭头三角形：弧末端（-20°）沿切线方向收敛的尖角
    tri = (polar(CX, CY, 93, -20), polar(CX, CY, 115, -20), polar(CX, CY, 104, 8))
    for py in range(w):
        for px_x in range(w):
            # 映射回设计坐标（连续值）
            x, y = (px_x + 0.5) / s, (py + 0.5) / s

            # 1) 背景：对角渐变 + 圆角裁剪（外部透明）
            if not in_rounded_rect(x, y, 0, 0, SIZE, SIZE, 56):
                px[py][px_x] = (0, 0, 0, 0)
                continue
            t = (x + y) / (2 * SIZE)
            color = lerp(GRAD_TL, GRAD_BR, t)

            # 2) 琥珀迁移环（半径 104、宽 15，扫过顶部到右侧 -20°，缺口在底部）
            dx, dy = x - CX, y - CY
            dist = math.hypot(dx, dy)
            if abs(dist - 104) <= 7.5:
                ang = math.degrees(math.atan2(dy, dx))  # -180..180，-90 为正上
                if -172 <= ang <= -20:
                    color = AMBER
            if in_triangle(x, y, *tri):
                color = AMBER

            # 3) 白色硬盘（圆角矩形 + 槽线 + 状态灯）
            if in_rounded_rect(x, y, 64, 96, 192, 168, 18):
                color = WHITE
                if in_rounded_rect(x, y, 86, 116, 148, 127, 5) or in_rounded_rect(x, y, 86, 137, 148, 148, 5):
                    color = SLOT
                if (x - 172) ** 2 + (y - 152) ** 2 <= 7 ** 2:
                    color = AMBER
            px[py][px_x] = color + (255,)
    return px


def write_png(path: str, w: int) -> None:
    # 超采样渲染后按 SS 块降采样（抗锯齿）。
    # RGBA 预乘平均：避免透明区黑色像素混入边缘产生黑描边。
    fine = render(w * SS)
    px = [[(0, 0, 0, 0)] * w for _ in range(w)]
    for oy in range(w):
        for ox in range(w):
            ra = ga = ba = aa = 0.0
            for dy in range(SS):
                for dx in range(SS):
                    c = fine[oy * SS + dy][ox * SS + dx]
                    a = c[3] / 255.0
                    ra += c[0] * a; ga += c[1] * a; ba += c[2] * a; aa += a
            n = SS * SS
            if aa > 0:
                px[oy][ox] = (
                    round(ra / aa), round(ga / aa), round(ba / aa),
                    round(255 * aa / n),
                )

    raw = b"".join(
        b"\x00" + b"".join(struct.pack("BBBB", *px[y][x]) for x in range(w))
        for y in range(w)
    )

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data)) + tag + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", w, w, 8, 6, 0, 0, 0)  # RGBA
    png = (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(raw, 9))
        + chunk(b"IEND", b"")
    )
    Path(path).write_bytes(png)
    print(f"written {path} ({w}x{w}, {len(png)} bytes)")


if __name__ == "__main__":
    root = Path(__file__).resolve().parent.parent
    write_png(str(root / "ICON.PNG"), 64)
    write_png(str(root / "ICON_256.PNG"), 256)
