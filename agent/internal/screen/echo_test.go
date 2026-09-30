package screen

import "testing"

// firstNonBlankCol 返回一行里第一个非空格格的列号（没有则 -1）。
func firstNonBlankCol(s *Screen, y int) int {
	if y < 0 || y >= s.gridRows() {
		return -1
	}
	row := s.grid[y*s.cols : (y+1)*s.cols]
	for i, c := range row {
		if c.r != 0 && c.r != ' ' {
			return i
		}
	}
	return -1
}

// findRowContaining 找出内容区里包含指定文本的行号（-1 表示没有）。
func findRowContaining(s *Screen, want string) int {
	for y := 0; y < s.rows; y++ {
		if contains(s.textRow(y), want) {
			return y
		}
	}
	return -1
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// 回车后的回显必须从内容区第 0 列开始：固定输入行有自己的光标列，
// 不能因为画输入行就把内容光标（及其列号）留在输入行上。
func TestEchoStartsAtColumnZero(t *testing.T) {
	s, _ := testScreen(t, 40, 10)
	s.SetStatusRows(2)
	s.Write([]byte("已有内容\n"))

	ed := &lineEditor{s: s, prompt: []rune("你> "), buf: []rune("abc")}
	ed.redraw()
	ed.clearRow()
	s.HideCursor()
	ed.echo()

	row := findRowContaining(s, "你> abc")
	if row < 0 {
		t.Fatal("内容区里找不到回显行")
	}
	if got := firstNonBlankCol(s, row); got != 0 {
		t.Fatalf("回显应从第 0 列开始，实际从第 %d 列开始", got)
	}
}

// 画固定输入行不能改动内容流光标：后续内容必须从第 0 列继续。
func TestInputRowKeepsContentCursor(t *testing.T) {
	s, _ := testScreen(t, 40, 10)
	s.SetStatusRows(2)
	s.Write([]byte("第一行\n"))

	ed := &lineEditor{s: s, prompt: []rune("你> "), buf: []rune("写点东西")}
	ed.redraw()

	// 输入行画完后，内容流继续写：必须落在新行的第 0 列
	s.Write([]byte("第二行\n"))
	row := findRowContaining(s, "第二行")
	if row < 0 {
		t.Fatal("内容区里找不到新的内容行")
	}
	if got := firstNonBlankCol(s, row); got != 0 {
		t.Fatalf("输入行不应影响内容光标，第二行应从第 0 列开始，实际第 %d 列", got)
	}
}
