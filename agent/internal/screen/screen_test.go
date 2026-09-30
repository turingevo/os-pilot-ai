package screen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func colFgDefault() uint32 { return resolveFg(cDefault, false) }

func TestWriteASCIIAndNewline(t *testing.T) {
	s, _ := testScreen(t, 10, 3)
	s.Write([]byte("abc\ndef"))
	if got := s.textRow(0)[:3]; got != "abc" {
		t.Fatalf("row0=%q", s.textRow(0))
	}
	if got := s.textRow(1)[:3]; got != "def" {
		t.Fatalf("row1=%q", s.textRow(1))
	}
	if s.curX != 3 || s.curY != 1 {
		t.Fatalf("cursor=%d,%d", s.curX, s.curY)
	}
}

func TestSGRColors(t *testing.T) {
	s, _ := testScreen(t, 10, 3)
	s.Write([]byte("\x1b[31mR\x1b[0mG\n"))
	if fg := s.grid[s.idx(0, 0)].fg; fg != resolveFg(cRed, false) {
		t.Fatalf("红色前景: %06X", fg)
	}
	if fg := s.grid[s.idx(1, 0)].fg; fg != resolveFg(cDefault, false) {
		t.Fatalf("reset 后应为默认色: %06X", fg)
	}
	s.Write([]byte("\x1b[2mD\x1b[22mN\n"))
	if fg := s.grid[s.idx(0, 1)].fg; fg != resolveFg(cDefault, true) {
		t.Fatalf("dim: %06X", fg)
	}
	if fg := s.grid[s.idx(1, 1)].fg; fg != resolveFg(cDefault, false) {
		t.Fatalf("22 应恢复正常: %06X", fg)
	}
	// 32/36
	s.Write([]byte("\x1b[32mG\x1b[36mC"))
	if s.grid[s.idx(0, 2)].fg != resolveFg(cGreen, false) || s.grid[s.idx(1, 2)].fg != resolveFg(cCyan, false) {
		t.Fatal("绿/青颜色不符")
	}
}

func TestWideCharGridAndPixels(t *testing.T) {
	s, cv := testScreen(t, 10, 3)
	s.Write([]byte("中A"))
	if !s.grid[s.idx(0, 0)].wide || !s.grid[s.idx(1, 0)].cont {
		t.Fatal("中 应占 2 格（左宽右续）")
	}
	if s.grid[s.idx(2, 0)].r != 'A' || s.curX != 3 {
		t.Fatalf("A 应在第 3 格: r=%q curX=%d", s.grid[s.idx(2, 0)].r, s.curX)
	}
	// 宽字符像素覆盖 16px
	if cv.Pixel(0, 0) != colFgDefault() || cv.Pixel(15, 0) != colFgDefault() {
		t.Fatalf("中 像素未覆盖 16px: %06X %06X", cv.Pixel(0, 0), cv.Pixel(15, 0))
	}
	// A 在下一格（x=16 起），其右侧相邻格应为空白
	if cv.Pixel(16, 0) != colFgDefault() || cv.Pixel(24, 0) != colBg {
		t.Fatalf("A 像素范围不符: %06X %06X", cv.Pixel(16, 0), cv.Pixel(24, 0))
	}
}

func TestOverwriteWideByNarrow(t *testing.T) {
	s, cv := testScreen(t, 10, 3)
	s.Write([]byte("中\rAB")) // B 的字形为全空，便于检测宽字符残留像素
	if s.grid[s.idx(0, 0)].wide || s.grid[s.idx(1, 0)].cont {
		t.Fatal("写窄字符应清除宽字符痕迹")
	}
	if s.grid[s.idx(0, 0)].r != 'A' || s.grid[s.idx(1, 0)].r != 'B' {
		t.Fatalf("grid: %q %q", s.grid[s.idx(0, 0)].r, s.grid[s.idx(1, 0)].r)
	}
	for x := 8; x < 16; x++ {
		if cv.Pixel(x, 0) != colBg {
			t.Fatalf("第 2 格残留宽字符像素 x=%d: %06X", x, cv.Pixel(x, 0))
		}
	}
}

func TestWrapAndScroll(t *testing.T) {
	s, cv := testScreen(t, 4, 2)
	s.Write([]byte("abcdEFGH"))
	if s.scrollN != 0 {
		t.Fatalf("换行是延迟滚动的，此时不应滚动: %d", s.scrollN)
	}
	s.Write([]byte("ij")) // 触发落地滚动
	if s.curX != 2 || s.curY != 1 || s.scrollN != 1 {
		t.Fatalf("cur=%d,%d scrollN=%d", s.curX, s.curY, s.scrollN)
	}
	row0, row1 := s.textRow(0), s.textRow(1)
	if !strings.HasPrefix(row0, "EFGH") || !strings.HasPrefix(row1, "ij") {
		t.Fatalf("滚动后内容: row0=%q row1=%q", row0, row1)
	}
	// 滚动像素也应整体上移
	if cv.Pixel(0, 0) == colBg && cv.Pixel(0, 16) == colBg {
		t.Fatal("滚动后像素未上移")
	}
}

func TestWideWrapAtLastColumn(t *testing.T) {
	s, _ := testScreen(t, 4, 2)
	s.Write([]byte("abc")) // curX=3，仅剩 1 格
	s.Write([]byte("中"))
	if s.grid[s.idx(3, 0)].r != ' ' {
		t.Fatal("行尾不足 2 格时宽字符应换行")
	}
	if s.grid[s.idx(0, 1)].r != '中' || s.curY != 1 || s.curX != 2 {
		t.Fatalf("cur=%d,%d", s.curX, s.curY)
	}
}

func TestCursorInvert(t *testing.T) {
	s, cv := testScreen(t, 10, 3)
	s.Write([]byte("A"))
	if cv.Pixel(0, 0) != colFgDefault() {
		t.Fatal("A 应为前景色像素")
	}
	s.ShowCursor() // cursor 在 A 之后一格（空白格）
	if cv.Pixel(8, 0) != colFgDefault() {
		t.Fatalf("空白格上的光标应显示为前景色块: %06X", cv.Pixel(8, 0))
	}
	s.setCursor(0, 0)
	s.HideCursor() // 旧位置恢复
	s.ShowCursor()
	if cv.Pixel(0, 0) != colBg || cv.Pixel(7, 0) != colBg {
		t.Fatalf("A 上的光标应反白（前景变背景）: %06X %06X", cv.Pixel(0, 0), cv.Pixel(7, 0))
	}
	s.HideCursor()
	if cv.Pixel(0, 0) != colFgDefault() {
		t.Fatal("隐藏光标后应恢复")
	}
}

func TestWideCharCursorCoversBothCells(t *testing.T) {
	s, cv := testScreen(t, 10, 3)
	s.Write([]byte("中"))
	s.setCursor(1, 0) // 光标落在宽字符右半格
	s.ShowCursor()
	if cv.Pixel(0, 0) != colBg || cv.Pixel(15, 0) != colBg {
		t.Fatalf("宽字符光标应覆盖整字: %06X %06X", cv.Pixel(0, 0), cv.Pixel(15, 0))
	}
}

func TestEraseToEOSAndCSI(t *testing.T) {
	s, cv := testScreen(t, 6, 3)
	s.Write([]byte("aaa\nbbb\nccc"))
	s.setCursor(2, 1)
	s.eraseToEOS()
	if s.textRow(1) != "bb    " || strings.TrimSpace(s.textRow(2)) != "" {
		t.Fatalf("row1=%q row2=%q", s.textRow(1), s.textRow(2))
	}
	if cv.Pixel(3*cellW, cellH) != colBg {
		t.Fatal("擦除后像素应为背景色")
	}
	// CSI 2J 清屏
	s.Write([]byte("\x1b[2J"))
	if strings.TrimSpace(s.textRow(0)) != "" {
		t.Fatalf("2J 后 row0=%q", s.textRow(0))
	}
	// CSI K 行内擦除
	s.Write([]byte("xyz\x1b[2K"))
	if strings.TrimSpace(s.textRow(s.curY)) != "" {
		t.Fatalf("2K 后当前行=%q", s.textRow(s.curY))
	}
}

func TestTabAndBackspace(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	s.Write([]byte("ab\tc"))
	if s.curX != 9 {
		t.Fatalf("\t 后应到第 8 格: %d", s.curX)
	}
	s.Write([]byte("\b\b"))
	if s.curX != 7 {
		t.Fatalf("\\b 后 curX=%d", s.curX)
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	s, _ := testScreen(t, 10, 3)
	b := []byte("中")
	// 分两次写入一个 UTF-8 字符（模拟流式输出的分片）
	s.Write(b[:1])
	s.Write(b[1:])
	if s.grid[s.idx(0, 0)].r != '中' {
		t.Fatalf("分片 UTF-8 应正确合并: %q", s.grid[s.idx(0, 0)].r)
	}
}

func TestInvalidUTF8(t *testing.T) {
	s, _ := testScreen(t, 10, 3)
	s.Write([]byte{'a', 0xE4, 'b'}) // 0xE4 后跟非续字节
	if s.grid[s.idx(0, 0)].r != 'a' {
		t.Fatalf("grid[0]=%q", s.grid[s.idx(0, 0)].r)
	}
	if s.grid[s.idx(1, 0)].r != '?' || s.grid[s.idx(2, 0)].r != 'b' {
		t.Fatalf("非法字节应回退 '?' 并继续: %q %q", s.grid[s.idx(1, 0)].r, s.grid[s.idx(2, 0)].r)
	}
}

// 用真实字体渲染一屏内容并导出 PPM（人工可查；同时验证中文区域有像素）
func TestRealFontScreenshot(t *testing.T) {
	f := realFont(t)
	cv := newMemCanvas(80*cellW, 25*cellH)
	s := newScreen(cv, f)
	s.Write([]byte("\x1b[1m================ AI 装机助手 ================\x1b[0m\n"))
	s.Write([]byte("模型: mock-model | 模式: orchestrate | 数据分区: /iso\n"))
	s.Write([]byte("你> 帮我给 ubuntu-24.04 配置自动安装\n"))
	s.Write([]byte("\x1b[36m\x1b[1mAI>\x1b[0m 好的，我先探测 U 盘上的镜像与应答脚本（← 箭头符号 ✓）\n"))
	s.Write([]byte("  → system_probe  \x1b[32m← 完成\x1b[0m · 0.03s\n"))
	s.Write([]byte("    exit_code=0\n"))
	s.Write([]byte("（思考中）\n"))
	s.Write([]byte("正在扫描 /iso 目录…\n"))
	s.ShowCursor()

	// 中文“装机助手”位于 (0,0) 行的第 ... 直接用文本断言
	if row := s.textRow(0); !strings.Contains(row, "AI 装机助手") {
		t.Fatalf("row0=%q", row)
	}
	// 找“装”字的像素：整行中应存在前景像素
	found := false
	for x := 0; x < 80*cellW && !found; x++ {
		for y := 0; y < cellH; y++ {
			if cv.Pixel(x, y) != colBg {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("渲染后应有前景像素")
	}
	ppm := filepath.Join(t.TempDir(), "screen.ppm")
	if err := cv.WritePPM(ppm); err != nil {
		t.Fatalf("WritePPM: %v", err)
	}
	if out := os.Getenv("VTOY_SCREEN_PPM"); out != "" { // 人工查看渲染效果
		if err := cv.WritePPM(out); err != nil {
			t.Fatalf("WritePPM(%s): %v", out, err)
		}
	}
}
