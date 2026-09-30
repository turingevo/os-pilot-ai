package screen

import (
	"errors"
	"strings"
	"testing"
)

type fakeIME struct {
	cands    map[string][]string
	errAt    string
	searches []string
	chosen   []int
	resets   int
	closed   bool
}

func (f *fakeIME) Search(py string) ([]string, error) {
	f.searches = append(f.searches, py)
	if py == f.errAt {
		return nil, errors.New("boom")
	}
	return f.cands[py], nil
}

func (f *fakeIME) Choose(i int) { f.chosen = append(f.chosen, i) }
func (f *fakeIME) Reset()       { f.resets++ }
func (f *fakeIME) Close()       { f.closed = true }

func imeScreen(t *testing.T, cols, rows int, f *fakeIME) *Screen {
	t.Helper()
	s, _ := testScreen(t, cols, rows)
	s.SetStatusRows(2) // 底部留固定输入行 + 状态栏，行编辑才有落点
	s.ime = f
	return s
}

// newEditor 构造一个绑定在固定输入行上的行编辑器（渲染断言用）。
func newEditor(s *Screen, prompt string) *lineEditor {
	return &lineEditor{s: s, prompt: []rune(prompt)}
}

func TestIMEToggleAndModePersists(t *testing.T) {
	f := &fakeIME{cands: map[string][]string{"bang": {"帮"}}}
	s := imeScreen(t, 40, 3, f)
	line, err := readLine(t, s, "\x00bang\r", "你> ")
	if err != nil || line != "帮" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if !s.imeOn {
		t.Fatal("Ctrl-Space 应开启输入法")
	}
	// 第二行无需再按 Ctrl-Space（中英状态跨行保持）
	line, err = readLine(t, s, "bang\r", "你> ")
	if err != nil || line != "帮" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	// 再按一次关闭：普通 ASCII 输入不受影响
	line, err = readLine(t, s, "\x00ok\r", "你> ")
	if err != nil || line != "ok" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if s.imeOn {
		t.Fatal("再次 Ctrl-Space 应关闭输入法")
	}
}

func TestIMESuffixRendering(t *testing.T) {
	f := &fakeIME{cands: map[string][]string{"bangwo": {"帮我", "帮", "邦"}}}
	s := imeScreen(t, 40, 3, f)
	s.imeOn = true
	ed := newEditor(s, "你> ")
	ed.comp = []rune("bangwo")
	ed.cands = []string{"帮我", "帮", "邦"}
	ed.redraw()

	row := s.inputRowText()
	for _, want := range []string{"bangwo", "1.帮我", "2.帮", "3.邦"} {
		if !strings.Contains(row, want) {
			t.Fatalf("候选条缺少 %q: %q", want, row)
		}
	}
	if s.inputCurX != 3 { // 光标仍在 buf 末尾（prompt 之后），不随后缀移动
		t.Fatalf("光标应在 prompt 末尾: %d", s.inputCurX)
	}

	// 无组合：状态标记 [中]
	ed.comp, ed.cands = nil, nil
	ed.redraw()
	if row := s.inputRowText(); !strings.Contains(row, "[中]") {
		t.Fatalf("应显示 [中]: %q", row)
	}
	// 后缀超宽被裁剪后光标仍在屏内（固定输入行不换行、不滚动内容）
	s2 := imeScreen(t, 10, 3, f)
	s2.imeOn = true
	ed2 := newEditor(s2, "P> ")
	ed2.comp = []rune("bangwozhuangxitong")
	ed2.cands = []string{"帮我装系统", "帮我", "邦我装系统", "帮", "棒"}
	ed2.redraw()
	if !s2.inputCursor || s2.inputCurX < 0 || s2.inputCurX >= s2.cols {
		t.Fatalf("光标越界: %d,在输入行=%v（输入行 %d）", s2.inputCurX, s2.inputCursor, s2.inputRow)
	}
}

func TestIMECommitPaths(t *testing.T) {
	// 空格提交首选
	f := &fakeIME{cands: map[string][]string{"bangwo": {"帮我", "帮", "邦"}}}
	s := imeScreen(t, 40, 3, f)
	line, err := readLine(t, s, "\x00bangwo \r", "你> ")
	if err != nil || line != "帮我" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if len(f.chosen) != 1 || f.chosen[0] != 0 {
		t.Fatalf("应提交候选0: %v", f.chosen)
	}
	rows := s.textRow(0) + s.textRow(1) + s.textRow(2)
	if strings.Contains(rows, "1.帮我") || strings.Contains(rows, "[中]") {
		t.Fatalf("行结束后后缀不应残留: %q", rows)
	}

	// 数字选第 2 个候选（索引 1）
	f2 := &fakeIME{cands: map[string][]string{"bangwo": {"帮我", "帮", "邦"}}}
	s2 := imeScreen(t, 40, 3, f2)
	line, err = readLine(t, s2, "\x00bangwo2\r", "你> ")
	if err != nil || line != "帮" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if len(f2.chosen) != 1 || f2.chosen[0] != 1 {
		t.Fatalf("应提交候选1: %v", f2.chosen)
	}

	// 直接回车：提交首选词并送行
	f3 := &fakeIME{cands: map[string][]string{"qingxian": {"请先"}}}
	s3 := imeScreen(t, 40, 3, f3)
	line, err = readLine(t, s3, "\x00qingxian\r", "你> ")
	if err != nil || line != "请先" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestIMEBackspaceAndRawCommit(t *testing.T) {
	f := &fakeIME{cands: map[string][]string{"bang": {"帮"}}}
	s := imeScreen(t, 40, 3, f)
	// bango ← 退格去掉 o → bang，回车提交
	line, err := readLine(t, s, "\x00bango\x7f\r", "你> ")
	if err != nil || line != "帮" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if len(f.searches) == 0 || f.searches[len(f.searches)-1] != "bang" {
		t.Fatalf("退格后应重新检索 bang: %v", f.searches)
	}
	// 乱敲无候选：回车原样上屏
	f2 := &fakeIME{}
	s2 := imeScreen(t, 40, 3, f2)
	line, err = readLine(t, s2, "\x00zzz\r", "你> ")
	if err != nil || line != "zzz" {
		t.Fatalf("无候选应原样上屏: line=%q err=%v", line, err)
	}
	// 空组合退格：交给普通删除（删掉 buf 中已有字符）
	f3 := &fakeIME{}
	s3 := imeScreen(t, 40, 3, f3)
	line, err = readLine(t, s3, "\x00abc\x7f\r", "你> ")
	if err != nil || line != "ab" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestIMEUppercaseBypassAndAsciiPassthrough(t *testing.T) {
	f := &fakeIME{}
	s := imeScreen(t, 40, 3, f)
	// 大写直通；小写进入组合但无候选时空格原样上屏 → "Hello"
	line, err := readLine(t, s, "\x00Hello \r", "你> ")
	if err != nil || line != "Hello" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	want := []string{"e", "el", "ell", "ello"}
	if strings.Join(f.searches, ",") != strings.Join(want, ",") {
		t.Fatalf("检索序列: %v", f.searches)
	}
	// 无组合时数字/空格照常插入
	line, err = readLine(t, s, "3 \r", "你> ")
	if err != nil || line != "3 " {
		t.Fatalf("line=%q err=%v", line, err)
	}
}

func TestIMECtrlUClearsComposition(t *testing.T) {
	f := &fakeIME{cands: map[string][]string{"bangwo": {"帮我"}}}
	s := imeScreen(t, 40, 3, f)
	line, err := readLine(t, s, "\x00bangwo\x15\r", "你> ")
	if err != nil || line != "" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if f.resets == 0 {
		t.Fatal("清组合应通知引擎 reset")
	}
}

func TestIMECtrlCLeavesNoSuffix(t *testing.T) {
	f := &fakeIME{cands: map[string][]string{"bangwo": {"帮我", "帮"}}}
	s := imeScreen(t, 40, 3, f)
	line, err := readLine(t, s, "\x00bangwo\x03", "你> ")
	if err != nil || line != "" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	rows := s.textRow(0) + s.textRow(1)
	if !strings.Contains(rows, "^C") {
		t.Fatalf("应回显 ^C: %q", rows)
	}
	if strings.Contains(rows, "bangwo") || strings.Contains(rows, "[中]") {
		t.Fatalf("中断后后缀不应残留: %q", rows)
	}
}

func TestIMEUnavailableNotice(t *testing.T) {
	t.Setenv("VTOY_AI_IME_BIN", "/nonexistent/pinyin-ime")
	t.Setenv("VTOY_AI_IME_DICT", "/nonexistent/dict.dat")
	s, _ := testScreen(t, 60, 3)
	s.SetStatusRows(2)
	ed := newEditor(s, "你> ")
	ed.toggleIME()
	if s.imeOn || !s.imeDead {
		t.Fatalf("组件缺失应提示并停止重试: on=%v dead=%v", s.imeOn, s.imeDead)
	}
	if row := s.inputRowText(); !strings.Contains(row, "输入法不可用") {
		t.Fatalf("应显示不可用提示: %q", row)
	}
	if IMEAvailable() {
		t.Fatal("组件缺失时 IMEAvailable 应为 false")
	}
}

func TestIMEEngineFailureDegrades(t *testing.T) {
	f := &fakeIME{errAt: "bad"}
	s := imeScreen(t, 60, 3, f)
	s.imeOn = true
	ed := newEditor(s, "你> ")
	ed.comp = []rune("bad")
	ed.imeRefresh()
	if s.imeOn || !s.imeDead {
		t.Fatalf("引擎失效应关闭输入法: on=%v dead=%v", s.imeOn, s.imeDead)
	}
	if ed.composing() || len(ed.cands) != 0 {
		t.Fatal("失效后应清空组合")
	}
	if row := s.inputRowText(); !strings.Contains(row, "输入法失效") {
		t.Fatalf("应显示一次性提示: %q", row)
	}
	// 再按 Ctrl-Space：提示不可用且不崩溃
	ed.toggleIME()
	if row := s.inputRowText(); !strings.Contains(row, "输入法不可用") {
		t.Fatalf("应提示不可用: %q", row)
	}
}

func TestScreenCloseClosesIME(t *testing.T) {
	f := &fakeIME{}
	s, _ := testScreen(t, 20, 3)
	s.ime = f
	s.Close()
	if !f.closed {
		t.Fatal("Close 应回收输入法引擎")
	}
	if s.ime != nil {
		t.Fatal("Close 后应清空引擎引用")
	}
}
