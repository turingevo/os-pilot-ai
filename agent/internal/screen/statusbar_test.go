package screen

import (
	"strings"
	"testing"
)

// 两行布局（生产配置 statusRows=2）：固定输入行在倒数第二行，状态栏在最后一行，
// 二者互不覆盖，输入行也不会被状态栏文本冲掉。
func TestStatusBarWithFixedInputRow(t *testing.T) {
	s, cv := testScreen(t, 20, 5)
	s.SetStatusRows(2) // 内容 3 行 + 输入行 + 状态栏
	if s.rows != 3 || s.inputRow != 3 {
		t.Fatalf("内容区=%d 输入行=%d（期望 3/3）", s.rows, s.inputRow)
	}
	s.SetStatus("STATUS")
	ed := newEditor(s, "P> ")
	ed.buf = []rune("hello")
	ed.redraw()

	countBg := func(y int) int {
		n := 0
		for x := 0; x < 20*cellW; x++ {
			if cv.Pixel(x, y*cellH) == statusBg {
				n++
			}
		}
		return n
	}
	if countBg(4) == 0 {
		t.Fatal("状态栏应在最后一行(4)")
	}
	if countBg(3) != 0 {
		t.Fatal("输入行(3)不应被状态栏底色覆盖")
	}
	if row := s.inputRowText(); !strings.Contains(row, "P> hello") {
		t.Fatalf("输入行文本被破坏: %q", row)
	}
	// 内容滚动不影响输入行与状态栏
	for i := 0; i < 10; i++ {
		s.Write([]byte("line\n"))
	}
	if row := s.inputRowText(); !strings.Contains(row, "P> hello") {
		t.Fatalf("滚动后输入行应保持: %q", row)
	}
	if countBg(4) == 0 {
		t.Fatal("滚动后状态栏不应被覆盖")
	}
}

// 状态栏：底部保留行 + 内容滚动不覆盖它。
func TestStatusBarReservedAndPainted(t *testing.T) {
	s, cv := testScreen(t, 20, 5)
	if s.Rows() != 5 {
		t.Fatalf("未保留时应占满整屏，得到 %d", s.Rows())
	}
	s.SetStatusRows(1)
	if s.Rows() != 4 {
		t.Fatalf("保留 1 行后内容区应为 4 行，得到 %d", s.Rows())
	}
	s.SetStatus(" [AI] test ")

	countBg := func(y int) int {
		n := 0
		for x := 0; x < 20*cellW; x++ {
			if cv.Pixel(x, y*cellH) == statusBg {
				n++
			}
		}
		return n
	}
	if countBg(4) == 0 {
		t.Fatal("状态栏所在行应被绘制（底色 0x000060）")
	}
	for i := 0; i < 20; i++ { // 写满并滚动
		s.Write([]byte("line\n"))
	}
	if countBg(4) == 0 {
		t.Fatal("内容滚动后状态栏不应被覆盖")
	}
	s.SetStatus(" 换文本 ")
	if !strings.Contains(s.status, "换文本") {
		t.Fatalf("状态栏文本未更新: %q", s.status)
	}
}

// Ctrl+T 切模式：返回当前输入、置位标志、清掉屏幕上的半截输入，并可携带到下一次。
func TestBackTabTogglesAndCarriesInput(t *testing.T) {
	s, _ := testScreen(t, 24, 4)
	line, err := readLine(t, s, "ls -l\x14", "你> ")
	if err != nil || line != "ls -l" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if !s.ConsumeTabToggle() {
		t.Fatal("应记录 Ctrl+T 切换")
	}
	if s.ConsumeTabToggle() {
		t.Fatal("切换标志应被消费（一次有效）")
	}
	if strings.Contains(visible(s), "ls -l") {
		t.Fatalf("切换后应清掉半截输入:\n%s", visible(s))
	}

	s.SetPrefill("ls -l") // 携带文本到下一次输入
	line, err = readLine(t, s, " /iso\r", "$ ")
	if err != nil || line != "ls -l /iso" {
		t.Fatalf("预填后 line=%q err=%v", line, err)
	}
	if s.ConsumeTabToggle() {
		t.Fatal("普通回车不应置切换标志")
	}
}
