package screen

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

func writeLines(s *Screen, n int) {
	for i := 0; i < n; i++ {
		s.Write([]byte("L" + strconv.Itoa(i) + "\n"))
	}
}

func visible(s *Screen) string {
	var b strings.Builder
	for y := 0; y < s.rows; y++ {
		b.WriteString(strings.TrimRight(s.visibleRow(y), " "))
		b.WriteString("\n")
	}
	return b.String()
}

// rowPixels 取第 y 行的像素快照（用于断言"回看中画面被冻结"）。
func rowPixels(cv *memCanvas, y, cells int) []byte {
	out := make([]byte, 0, cells*cellW*4)
	for x := 0; x < cells*cellW; x++ {
		off := y*cellH*cv.stride + x*4
		out = append(out, cv.buf[off:off+4]...)
	}
	return out
}

func TestScrollbackScrollsBackToOldest(t *testing.T) {
	s, _ := testScreen(t, 20, 5)
	writeLines(s, 30)
	if len(s.hist) == 0 {
		t.Fatal("应有历史行")
	}
	if got := strings.TrimRight(s.visibleRow(0), " "); got != "L25" {
		t.Fatalf("实时视图 row0=%q 期望 L25", got)
	}
	s.scrollBack(9999) // 顶到最旧
	if s.ViewOffset() != len(s.hist) {
		t.Fatalf("偏移应夹到最旧: got %d want %d", s.ViewOffset(), len(s.hist))
	}
	if got := strings.TrimRight(s.visibleRow(0), " "); got != "L0" {
		t.Fatalf("回看顶 row0=%q 期望 L0", got)
	}
	s.scrollToLive()
	if s.ViewOffset() != 0 {
		t.Fatalf("未回到实时视图: %d", s.ViewOffset())
	}
	if got := strings.TrimRight(s.visibleRow(4), " "); got != "L29" {
		t.Fatalf("实时视图 row4=%q 期望 L29", got)
	}
}

func TestScrollbackFreezesLiveOutput(t *testing.T) {
	s, cv := testScreen(t, 20, 5)
	writeLines(s, 12)
	s.scrollBack(3)
	if s.ViewOffset() != 3 {
		t.Fatalf("偏移=%d", s.ViewOffset())
	}
	before := rowPixels(cv, 0, s.cols)
	histN := len(s.hist)

	s.Write([]byte("NEW\n")) // 实时输出到达：画面必须冻结，缓冲继续增长

	if got := rowPixels(cv, 0, s.cols); !bytes.Equal(before, got) {
		t.Fatal("回看中画面不应被实时输出改写")
	}
	if len(s.hist) <= histN {
		t.Fatal("回看中历史仍应继续记录")
	}
	s.scrollToLive()
	if !strings.Contains(visible(s), "NEW") {
		t.Fatalf("回到实时视图应看到新输出:\n%s", visible(s))
	}
}

func TestScrollbackIndicatorPixels(t *testing.T) {
	s, cv := testScreen(t, 20, 4)
	writeLines(s, 8)
	s.scrollBack(2)
	found := false
	for x := 0; x < s.cols*cellW && !found; x++ {
		for y := 0; y < cellH; y++ {
			if cv.Pixel(x, y) == 0x000060 { // 提示条底色
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("回看时应在首行画出偏移提示")
	}
	s.scrollToLive()
	for x := 0; x < s.cols*cellW; x++ {
		for y := 0; y < cellH; y++ {
			if cv.Pixel(x, y) == 0x000060 {
				t.Fatalf("回到实时视图后提示应消失: (%d,%d)", x, y)
			}
		}
	}
}

func TestScrollbackCapAndClamp(t *testing.T) {
	s, _ := testScreen(t, 20, 3)
	for i := 0; i < maxScrollback+50; i++ {
		s.Write([]byte("x\n"))
	}
	if len(s.hist) != maxScrollback {
		t.Fatalf("历史应封顶 %d，实际 %d", maxScrollback, len(s.hist))
	}
	s.scrollBack(-5) // 实时视图下反向滚动应无副作用
	if s.ViewOffset() != 0 {
		t.Fatalf("实时视图下偏移应为 0: %d", s.ViewOffset())
	}
	s.scrollBack(maxScrollback * 2)
	s.scrollBack(-(maxScrollback * 2))
	if s.ViewOffset() != 0 {
		t.Fatalf("往返滚动后应回到实时: %d", s.ViewOffset())
	}
}

func TestScrollKeyParsing(t *testing.T) {
	if k := escKey('~', []byte("5")); k.kind != keyPageUp {
		t.Fatalf("ESC[5~ 应为 PageUp，得到 %v", k.kind)
	}
	if k := escKey('~', []byte("6")); k.kind != keyPageDown {
		t.Fatalf("ESC[6~ 应为 PageDown，得到 %v", k.kind)
	}
	if k := escKey('A', []byte("1;2")); k.kind != keyScrollUp {
		t.Fatalf("Shift+↑ 应为回看上滚，得到 %v", k.kind)
	}
	if k := escKey('B', []byte("1;2")); k.kind != keyScrollDown {
		t.Fatalf("Shift+↓ 应为回看下滚，得到 %v", k.kind)
	}
	// 原有映射不受影响
	if k := escKey('D', nil); k.kind != keyLeft {
		t.Fatalf("ESC[D 应仍为左移: %v", k.kind)
	}
	if k := escKey('~', []byte("3")); k.kind != keyDelete {
		t.Fatalf("ESC[3~ 应仍为 Delete: %v", k.kind)
	}
	if k := escKey('A', nil); k.kind != keyUp {
		t.Fatalf("裸上箭头应为历史浏览（回看走 Shift+↑）: %v", k.kind)
	}
}

func TestReadLineScrollKeysThenTyping(t *testing.T) {
	s, _ := testScreen(t, 20, 4)
	writeLines(s, 10)
	// PageUp 两次回看，再输入 hi 回车：翻页键不应进入输入行，且输入后自动回到实时视图
	line, err := readLine(t, s, "\x1b[5~\x1b[5~hi\r", "> ")
	if err != nil || line != "hi" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if s.ViewOffset() != 0 {
		t.Fatalf("输入后应回到实时视图: %d", s.ViewOffset())
	}
	if !strings.Contains(visible(s), "> hi") {
		t.Fatalf("回到实时视图后应看到输入行:\n%s", visible(s))
	}
}

// pollInput 是"流式输出期间也能翻页"的入口：翻页键立即生效，其余键缓存给下一次 ReadLine。
func TestPollInputScrollsAndBuffers(t *testing.T) {
	s, _ := testScreen(t, 20, 4)
	writeLines(s, 10)
	s.keys = make(chan key, 8) // 直接喂通道，不依赖后台 goroutine
	s.keys <- key{kind: keyPageUp}
	s.keys <- key{kind: keyPageUp}
	s.keys <- key{kind: keyRune, r: 'x'}

	s.pollInput()

	if s.ViewOffset() == 0 {
		t.Fatal("流式输出期间按 PgUp 应立即生效")
	}
	if len(s.pending) != 1 || s.pending[0].r != 'x' {
		t.Fatalf("非翻页键应缓存: %+v", s.pending)
	}
	k, err := s.nextKey()
	if err != nil || k.kind != keyRune || k.r != 'x' {
		t.Fatalf("nextKey 应先取缓存: %+v %v", k, err)
	}
	close(s.keys)
	if _, err := s.nextKey(); err != io.EOF {
		t.Fatalf("读取通道关闭应返回 io.EOF: %v", err)
	}
}

func TestPollInputSkippedWhileEditing(t *testing.T) {
	s, _ := testScreen(t, 20, 4)
	writeLines(s, 10)
	s.keys = make(chan key, 2)
	s.keys <- key{kind: keyPageUp}

	s.editing = true
	s.pollInput()
	if s.ViewOffset() != 0 {
		t.Fatal("行编辑中不应轮询按键（重绘由编辑器驱动）")
	}
	if len(s.keys) != 1 {
		t.Fatal("按键应留在通道里等待 ReadLine")
	}

	s.editing = false
	s.pollInput()
	if s.ViewOffset() == 0 {
		t.Fatal("非编辑态应处理翻页键")
	}
}

func TestHandleScrollKeyIgnoresOtherKeys(t *testing.T) {
	s, _ := testScreen(t, 20, 4)
	writeLines(s, 10)
	if s.handleScrollKey(key{kind: keyEnter}) {
		t.Fatal("回车不应被当成翻页键")
	}
	if !s.handleScrollKey(key{kind: keyPageUp}) {
		t.Fatal("PgUp 应被消费")
	}
	if !s.handleScrollKey(key{kind: keyScrollDown}) {
		t.Fatal("Shift+↓ 应被消费")
	}
}
