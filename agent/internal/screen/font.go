// Package screen 实现 console 上的用户态自绘屏幕：
// 直接把点阵字体画到 /dev/fb0（帧缓冲），绕开内核 VT/fbcon 无法渲染 CJK 的限制
// （内核字体最多 512 字形，且不支持混合宽度）。仅在“/dev/console 就是本地显示器”
// 时启用，串口路径不受影响。
package screen

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	glyphBytes   = 32 // 16 行 × 2 字节
	fontIndexLen = 12 // {codepoint u32, off u32, flags u32}
	fontMagic    = "VTF1"
	fontDefault  = "/ventoy/ai/screen/font.bin"
)

// Glyph 是一个 16x16 点阵字形；窄字形（8x16）存在每行高 8 位，左侧对齐。
type Glyph struct {
	Bits [glyphBytes]byte
	Wide bool
}

// Font 是 font.bin 的只读视图（索引 + 位图数据），由 pack/make_font.sh 生成，
// 作为独立数据文件随 initramfs 分发（不嵌入 agent 二进制）。
type Font struct {
	index    []byte
	blob     []byte
	count    int
	fallback Glyph
}

// LoadFont 加载点阵字体文件（随 initramfs 携带；VTOY_AI_FONT 可覆盖路径）。
func LoadFont() (*Font, error) {
	path := os.Getenv("VTOY_AI_FONT")
	if path == "" {
		path = fontDefault
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取字体 %s: %w", path, err)
	}
	f, err := parseFont(data)
	if err != nil {
		return nil, fmt.Errorf("字体 %s: %w", path, err)
	}
	return f, nil
}

func parseFont(data []byte) (*Font, error) {
	if len(data) < 8 || string(data[:4]) != fontMagic {
		return nil, fmt.Errorf("字体数据格式不正确")
	}
	count := int(binary.LittleEndian.Uint32(data[4:8]))
	need := 8 + count*fontIndexLen
	if count <= 0 || len(data) < need {
		return nil, fmt.Errorf("字体数据损坏: count=%d len=%d", count, len(data))
	}
	f := &Font{index: data[8:need], blob: data[need:], count: count}
	if g, ok := f.lookup('?'); ok {
		f.fallback = g
	}
	return f, nil
}

// Lookup 返回字符字形；缺字回退 '?'（再缺则返回空字形）。
func (f *Font) Lookup(r rune) (Glyph, bool) {
	g, ok := f.lookup(r)
	if !ok {
		return f.fallback, false
	}
	return g, true
}

func (f *Font) lookup(r rune) (Glyph, bool) {
	lo, hi := 0, f.count-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		e := f.index[mid*fontIndexLen:]
		cp := rune(binary.LittleEndian.Uint32(e[0:4]))
		switch {
		case r == cp:
			off := int(binary.LittleEndian.Uint32(e[4:8]))
			if off < 0 || off+glyphBytes > len(f.blob) {
				return Glyph{}, false
			}
			var g Glyph
			copy(g.Bits[:], f.blob[off:off+glyphBytes])
			g.Wide = binary.LittleEndian.Uint32(e[8:12])&1 == 1
			return g, true
		case r < cp:
			hi = mid - 1
		default:
			lo = mid + 1
		}
	}
	return Glyph{}, false
}

// Width 返回字符占用的显示单元数（1=窄，2=宽）。
func (f *Font) Width(r rune) int {
	if g, ok := f.lookup(r); ok && g.Wide {
		return 2
	}
	return 1
}
