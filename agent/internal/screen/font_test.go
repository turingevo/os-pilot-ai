package screen

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type testGlyph struct {
	cp   rune
	wide bool
	rows [16]uint16 // 每行 16 px，MSB 在左
}

func solidGlyph(cp rune, wide bool) testGlyph {
	g := testGlyph{cp: cp, wide: wide}
	for i := range g.rows {
		g.rows[i] = 0xFFFF
	}
	return g
}

func buildFontData(gs []testGlyph) []byte {
	sort.Slice(gs, func(i, j int) bool { return gs[i].cp < gs[j].cp })
	var idx, blob bytes.Buffer
	for _, g := range gs {
		var hdr [12]byte
		binary.LittleEndian.PutUint32(hdr[0:4], uint32(g.cp))
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(blob.Len()))
		var fl uint32
		if g.wide {
			fl = 1
		}
		binary.LittleEndian.PutUint32(hdr[8:12], fl)
		idx.Write(hdr[:])
		for _, r := range g.rows {
			blob.WriteByte(byte(r >> 8))
			blob.WriteByte(byte(r))
		}
	}
	out := []byte("VTF1")
	var cnt [4]byte
	binary.LittleEndian.PutUint32(cnt[:], uint32(len(gs)))
	out = append(out, cnt[:]...)
	out = append(out, idx.Bytes()...)
	return append(out, blob.Bytes()...)
}

func testGlyphs() []testGlyph {
	space := testGlyph{cp: ' '}
	q := solidGlyph('?', false)
	// 中文样本：全亮/顶部两点/单点，便于像素断言
	zhong := solidGlyph('中', true)
	wen := testGlyph{cp: '文', wide: true}
	wen.rows[0] = 0xFC00
	hao := testGlyph{cp: '好', wide: true}
	hao.rows[0] = 0x8000
	a := solidGlyph('a', false)
	x := solidGlyph('X', false)
	return []testGlyph{space, q, zhong, wen, hao, a, x,
		solidGlyph('A', false), testGlyph{cp: 'B'}, solidGlyph('c', false),
		solidGlyph('h', false), solidGlyph('i', false)}
}

func testFont(t *testing.T) *Font {
	t.Helper()
	f, err := parseFont(buildFontData(testGlyphs()))
	if err != nil {
		t.Fatalf("parseFont: %v", err)
	}
	return f
}

func testScreen(t *testing.T, cols, rows int) (*Screen, *memCanvas) {
	t.Helper()
	cv := newMemCanvas(cols*cellW, rows*cellH)
	return newScreen(cv, testFont(t)), cv
}

func TestParseFontLookup(t *testing.T) {
	f := testFont(t)
	if g, ok := f.Lookup('中'); !ok || !g.Wide {
		t.Fatalf("中 应为宽字形: ok=%v wide=%v", ok, g.Wide)
	}
	if g, ok := f.Lookup('A'); !ok || g.Wide {
		t.Fatalf("A 应为窄字形: ok=%v wide=%v", ok, g.Wide)
	}
	if f.Width('中') != 2 || f.Width('A') != 1 {
		t.Fatalf("Width: 中=%d A=%d", f.Width('中'), f.Width('A'))
	}
	// 缺字回退 '?'
	if g, ok := f.Lookup('Z'); ok || g != f.fallback {
		t.Fatalf("缺字应回退 '?' 且 ok=false")
	}
}

func TestParseFontBadData(t *testing.T) {
	for _, d := range [][]byte{nil, []byte("VTF"), []byte("XXXX1234")} {
		if _, err := parseFont(d); err == nil {
			t.Fatalf("应报错: %q", d)
		}
	}
}

// 真实字体（构建期由 pack/make_font.sh 生成）：常用中文/符号覆盖与宽度标记
func TestRealFontHasCJK(t *testing.T) {
	f := realFont(t)
	for _, r := range []rune{'A', '你', '好', '，', '？', '→', '·', '…', '⏎'} {
		g, ok := f.Lookup(r)
		if !ok {
			t.Fatalf("真实字体缺少 %q (U+%04X)", r, r)
		}
		if (g.Wide && f.Width(r) != 2) || (!g.Wide && f.Width(r) != 1) {
			t.Fatalf("%q 宽度标记不一致: wide=%v", r, g.Wide)
		}
	}
	if g, _ := f.Lookup('你'); !g.Wide {
		t.Fatalf("汉字应为宽字形")
	}
}

// realFont 加载构建期真实字体（$BUILD/screen/font.bin）。字体是仓库外产物：
// 未构建过的环境跳过（先跑 pack/pack_env.sh，或用 VTOY_AI_FONT 指定路径）。
func realFont(t *testing.T) *Font {
	t.Helper()
	path := os.Getenv("VTOY_AI_FONT")
	if path == "" {
		script, err := filepath.Abs(filepath.Join("..", "..", "..", "pack", "defaults.sh"))
		if err != nil {
			t.Skipf("无法定位 pack/defaults.sh: %v", err)
		}
		out, err := exec.Command("sh", "-c",
			`. "$1" 2>/dev/null; printf %s "$VTOY_AI_BUILD_DIR"`, "sh", script).Output()
		if err != nil || len(out) == 0 {
			t.Skip("无法解析 VTOY_AI_BUILD_DIR（pack/defaults.sh）")
		}
		path = filepath.Join(strings.TrimSpace(string(out)), "screen", "font.bin")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("未找到构建期字体 %s（先运行 pack/pack_env.sh，或用 VTOY_AI_FONT 指定）", path)
	}
	t.Setenv("VTOY_AI_FONT", path)
	f, err := LoadFont()
	if err != nil {
		t.Fatalf("LoadFont(%s): %v", path, err)
	}
	return f
}
