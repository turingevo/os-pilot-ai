#!/usr/bin/env python3
"""unifont.hex -> font.bin：AI 装机助手自绘屏幕点阵字体

用法:
    python3 make_font.py <unifont.hex> <输出 font.bin> [--stats]

输出格式（小端序）:
    偏移 0   magic  'VTF1'
    偏移 4   count  u32              字形数
    偏移 8   索引   count × 12 字节  {codepoint u32, off u32, flags u32}
                                    按 codepoint 升序；off 相对数据区起点
    数据区   字形   count × 32 字节  16 行 × 2 字节（每行 MSB 在左）
                宽字形: 16x16；flags bit0=1
                窄字形: 源 8x16，存放为每行高 8 位（16x16 左对齐）
"""
import struct
import sys

# 收录的码点区间：覆盖 agent 输出实际用到的字符（ASCII/中文/中文标点/箭头/全角）
# 以及常见排版符号。未收录的字符渲染为 '?'。
RANGES = [
    (0x0020, 0x007E, "ASCII 可见字符"),
    (0x00A0, 0x00FF, "Latin-1 补充"),
    (0x0100, 0x017F, "拉丁扩展 A"),
    (0x2000, 0x206F, "通用标点（— … “ ” · 等）"),
    (0x2070, 0x209F, "上/下标"),
    (0x20A0, 0x20BF, "货币符号"),
    (0x2100, 0x214F, "字母式符号（℃ 等）"),
    (0x2190, 0x21FF, "箭头（← →）"),
    (0x2200, 0x22FF, "数学运算符（≈ ≤ ≥ × ÷）"),
    (0x2300, 0x23FF, "杂项技术符号（⏎ 等）"),
    (0x2460, 0x24FF, "带圈字符（①②③）"),
    (0x2500, 0x257F, "制表符"),
    (0x2580, 0x259F, "块元素（█ ▓ ▒ ░）"),
    (0x25A0, 0x25FF, "几何图形（◀ ▶）"),
    (0x2600, 0x26FF, "杂项符号（☑ ⚠ 等）"),
    (0x2700, 0x27BF, "装饰符号（✓ ✗ 等）"),
    (0x3000, 0x303F, "CJK 标点（。，、【】「」）"),
    (0x3040, 0x30FF, "平假名/片假名"),
    (0x3400, 0x4DBF, "CJK 扩展 A"),
    (0x4E00, 0x9FFF, "CJK 基本区"),
    (0xF900, 0xFAFF, "CJK 兼容表意文字"),
    (0xFE30, 0xFE4F, "CJK 兼容标点"),
    (0xFF00, 0xFFEF, "全角字符（：；！？（）等）"),
]


def in_ranges(cp):
    for lo, hi, _ in RANGES:
        if lo <= cp <= hi:
            return True
    return False


def parse_hex(path):
    glyphs = {}
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        for line in f:
            line = line.strip()
            if not line or ":" not in line:
                continue
            cp_s, bmp = line.split(":", 1)
            try:
                cp = int(cp_s, 16)
            except ValueError:
                continue
            glyphs[cp] = bmp.strip()
    return glyphs


def glyph_bytes(bmp):
    """返回 (32 字节位图, 是否宽字形)；无法识别返回 None。"""
    n = len(bmp)
    if n == 64:
        row_digits, wide = 4, True
    elif n == 32:
        row_digits, wide = 2, False
    else:
        return None
    out = bytearray(32)
    for row in range(16):
        s = bmp[row * row_digits:(row + 1) * row_digits]
        try:
            v = int(s, 16)
        except ValueError:
            return None
        if not wide:
            v <<= 8
        out[row * 2] = (v >> 8) & 0xFF
        out[row * 2 + 1] = v & 0xFF
    return bytes(out), wide


def main():
    args = [a for a in sys.argv[1:] if a != "--stats"]
    show_stats = "--stats" in sys.argv[1:]
    if len(args) != 2:
        print(__doc__)
        return 2
    src, dst = args

    glyphs = parse_hex(src)
    entries = []
    skipped = 0
    for cp in sorted(glyphs):
        if cp < 0x20 or not in_ranges(cp):
            continue
        conv = glyph_bytes(glyphs[cp])
        if conv is None:
            skipped += 1
            continue
        entries.append((cp, conv[0], conv[1]))

    index = bytearray()
    blob = bytearray()
    width_counts = [0, 0]
    for cp, gb, wide in entries:
        index += struct.pack("<III", cp, len(blob), 1 if wide else 0)
        blob += gb
        width_counts[1 if wide else 0] += 1

    with open(dst, "wb") as f:
        f.write(b"VTF1")
        f.write(struct.pack("<I", len(entries)))
        f.write(index)
        f.write(blob)

    print("[make_font] %s -> %s" % (src, dst))
    print("[make_font] 字形 %d（宽 %d / 窄 %d），跳过 %d，文件 %d 字节"
          % (len(entries), width_counts[1], width_counts[0], skipped,
             8 + len(index) + len(blob)))
    if show_stats:
        for lo, hi, label in RANGES:
            n = sum(1 for cp, _, _ in entries if lo <= cp <= hi)
            print("    U+%04X-U+%04X %-28s %5d" % (lo, hi, label, n))
    return 0


if __name__ == "__main__":
    sys.exit(main())
