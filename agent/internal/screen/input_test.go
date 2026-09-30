package screen

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func readLine(t *testing.T, s *Screen, keys string, prompt string) (string, error) {
	t.Helper()
	s.in = bytes.NewReader([]byte(keys))
	return s.ReadLine(prompt)
}

func TestReadLineBasic(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	line, err := readLine(t, s, "hello\r", "你> ")
	if err != nil || line != "hello" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	row0 := s.textRow(0)
	if !strings.Contains(row0, "你> hello") {
		t.Fatalf("row0=%q", row0)
	}
	if s.cursorVis {
		t.Fatal("回车后光标应隐藏")
	}
	if s.curY != 1 || s.curX != 0 {
		t.Fatalf("回车后应在下一行行首: %d,%d", s.curX, s.curY)
	}
}

func TestReadLineBackspaceAndCJK(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	line, err := readLine(t, s, "中文\x7f\r", "P> ")
	if err != nil || line != "中" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if row := s.textRow(0); !strings.Contains(row, "P> 中") || strings.Contains(row, "文") {
		t.Fatalf("row0=%q", row)
	}
}

func TestReadLineArrowEdit(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	// 输入 ab，左移一格，插入 X → aXb
	line, err := readLine(t, s, "ab\x1b[DX\r", "> ")
	if err != nil || line != "aXb" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// Home/End
	line, err = readLine(t, s, "xyz\x01Q\x05W\r", "> ")
	if err != nil || line != "QxyzW" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// Delete 键删除光标处字符（ESC[3~）
	line, err = readLine(t, s, "ab\x1b[D\x1b[3~\r", "> ")
	if err != nil || line != "a" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestReadLineCtrlKeys(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	// Ctrl-U 清行
	line, err := readLine(t, s, "garbage\x15ok\r", "> ")
	if err != nil || line != "ok" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// Ctrl-C 中断当前行，返回空串且无错误
	line, err = readLine(t, s, "abc\x03", "> ")
	if err != nil || line != "" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if row := s.textRow(1); !strings.Contains(row, "^C") {
		t.Fatalf("Ctrl-C 应回显 ^C: %q", row)
	}
	// Ctrl-D → EOF
	_, err = readLine(t, s, "\x04", "> ")
	if err != io.EOF {
		t.Fatalf("Ctrl-D 应返回 io.EOF: %v", err)
	}
}

func TestReadLineWrapLayout(t *testing.T) {
	// 3 行 × 10 列，prompt "P> "（3 格）
	s, cv := testScreen(t, 10, 3)
	line, err := readLine(t, s, "0123456789abcdefghijXYZ\r", "P> ")
	if err != nil || line != "0123456789abcdefghijXYZ" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	want := []string{"P> 0123456", "789abcdefg", "hijXYZ"}
	for y, w := range want {
		if got := strings.TrimRight(s.textRow(y), " "); got != w {
			t.Fatalf("row%d=%q want %q", y, got, w)
		}
	}
	_ = cv
}

func TestReadLineLongInputSlidesInFixedRow(t *testing.T) {
	// 10 列：输入 25 个字符远超一行。固定输入行只做水平窗口滑动，内容区不滚动。
	s, _ := testScreen(t, 10, 4)
	s.SetStatusRows(2) // 内容 2 行 + 固定输入行 + 状态栏
	input := "abcdefghijklmnopqrstuvwxy"
	ed := newEditor(s, "P> ")
	ed.buf = []rune(input)
	ed.cur = len(ed.buf)
	ed.redraw()
	if s.scrollN != 0 {
		t.Fatalf("固定输入行超宽不应滚动内容: scrollN=%d", s.scrollN)
	}
	// 窗口左移后应能看到输入尾部；总宽 = 3 + 25 = 28，可见窗口 = 最后 10 格
	if row := s.inputRowText(); !strings.Contains(row, "qrstuvwxy") {
		t.Fatalf("输入行应滑到尾部: %q", row)
	}
	if s.inputCurX != s.cols-1 || !s.inputCursor {
		t.Fatalf("光标应贴在输入行右端: %d,在输入行=%v", s.inputCurX, s.inputCursor)
	}
	if s.curY == s.inputRow {
		t.Fatal("画输入行不应把内容光标留在输入行上")
	}

	// 回车后整行回显到内容区（内容区仅 2 行，必然滚动）
	line, err := readLine(t, s, input+"\r", "P> ")
	if err != nil || line != input {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if s.scrollN == 0 {
		t.Fatal("回显超长行应触发内容滚动")
	}
}

func TestEditorRedrawCursorCell(t *testing.T) {
	s, _ := testScreen(t, 20, 4)
	s.SetStatusRows(2) // 内容区 2 行 + 固定输入行 + 状态栏
	ed := &lineEditor{s: s, prompt: []rune("> ")}
	ed.buf = []rune("a中b")
	ed.cur = 2 // 光标在 'b' 前："> "(2 格) + a(1) + 中(2) = 第 5 格
	ed.redraw()
	if s.inputCurX != 5 || !s.inputCursor {
		t.Fatalf("光标格=%d,在输入行=%v（输入行应为 %d）", s.inputCurX, s.inputCursor, s.inputRow)
	}
	if s.curY == s.inputRow {
		t.Fatal("画输入行不应把内容光标留在输入行上")
	}
	if !s.cursorVis {
		t.Fatal("编辑时应显示光标")
	}
	if row := s.inputRowText(); !strings.HasPrefix(row, "> a中b") {
		t.Fatalf("输入行=%q", row)
	}
	// 光标在末尾："> " + a + 中 + b = 第 6 格
	ed.cur = 3
	ed.redraw()
	if s.inputCurX != 6 || !s.inputCursor {
		t.Fatalf("光标格=%d,在输入行=%v", s.inputCurX, s.inputCursor)
	}
}

// 输入行变短（历史回退/退格/Ctrl-U）时，上一帧多出来的像素必须被清掉，
// 否则屏幕上会残留旧字符与旧光标块。
func TestEditorRedrawClearsStalePixels(t *testing.T) {
	s, cv := testScreen(t, 20, 3)
	s.SetStatusRows(2)
	ed := newEditor(s, "P> ")
	ed.buf = []rune("abcdefgh")
	ed.cur = len(ed.buf)
	ed.redraw()

	ed.buf = []rune("abc")
	ed.cur = len(ed.buf)
	ed.redraw()

	// prompt 占 3 格 + "abc" 3 格 = 第 6 格起是光标块；第 7 格之后必须全黑
	for x := 7; x < s.cols; x++ {
		for y := 0; y < cellH; y++ {
			if got := cv.Pixel(x*cellW, s.inputRow*cellH+y); got != 0 {
				t.Fatalf("第 %d 格残留旧像素: 0x%06X", x, got)
			}
		}
	}
}

func TestReadLineEOFOnInputClose(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	s.in = bytes.NewReader(nil)
	if _, err := s.ReadLine("> "); err != io.EOF {
		t.Fatalf("输入流结束应返回 io.EOF: %v", err)
	}
}

func TestMakeRawInvalidFD(t *testing.T) {
	if _, err := makeRaw(-1); err == nil {
		t.Fatal("无效 fd 应报错")
	}
}
