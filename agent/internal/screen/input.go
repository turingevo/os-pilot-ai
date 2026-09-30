package screen

import (
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// 便于在测试中替换具体类型
type (
	readerOnly = io.Reader
	ttyFile    = *os.File
	termiosT   = syscall.Termios
)

const (
	tcgets = 0x5401
	tcsets = 0x5402
)

// makeRaw 把 tty 切到 raw 模式（逐字节、无回显、无信号），返回原始设置。
func makeRaw(fd int) (termiosT, error) {
	var t termiosT
	if err := ioctlPtr(fd, tcgets, unsafe.Pointer(&t)); err != nil {
		return t, err
	}
	raw := t
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctlPtr(fd, tcsets, unsafe.Pointer(&raw)); err != nil {
		return t, err
	}
	return t, nil
}

func restoreTermios(fd int, t termiosT) {
	_ = ioctlPtr(fd, tcsets, unsafe.Pointer(&t))
}

type keyKind int

const (
	keyNone keyKind = iota
	keyRune
	keyEnter
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyClearLine // Ctrl-U
	keyInterrupt // Ctrl-C
	keyEOF       // Ctrl-D
	keyToggleIME // Ctrl-Space（0x00）

	// 回看（自绘屏幕）
	keyPageUp     // PageUp（ESC[5~）
	keyPageDown   // PageDown（ESC[6~）
	keyScrollUp   // Shift+↑（ESC[1;2A）
	keyScrollDown // Shift+↓（ESC[1;2B）

	keyTab     // Tab：命令补全
	keyBackTab // Shift+Tab（ESC[Z）：在 AI / 命令模式间切换

	keyUp   // ↑：取上一条历史
	keyDown // ↓：取下一条历史
)

type key struct {
	kind keyKind
	r    rune
}

func (s *Screen) readByte() (byte, error) {
	var b [1]byte
	for {
		n, err := s.in.Read(b[:])
		if n == 1 {
			return b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func (s *Screen) readKeyDirect() (key, error) {
	b, err := s.readByte()
	if err != nil {
		return key{kind: keyEOF}, err
	}
	switch {
	case b == 0x00:
		return key{kind: keyToggleIME}, nil
	case b == 0x03:
		return key{kind: keyInterrupt}, nil
	case b == 0x04:
		return key{kind: keyEOF}, nil
	case b == 0x15:
		return key{kind: keyClearLine}, nil
	case b == 0x09:
		return key{kind: keyTab}, nil
	case b == 0x14:
		// Ctrl+T：本地屏（Linux 控制台/VT）切模式。VT 的键映射不让 Shift+Tab 与 Tab 区分
		// （实测 sendkey shift-tab 与 Tab 同效），所以这里用 Ctrl+T；串口/终端路径的
		// Shift+Tab 由 ESC[Z 承担（见 escKey）。
		return key{kind: keyBackTab}, nil
	case b == 0x01:
		return key{kind: keyHome}, nil
	case b == 0x05:
		return key{kind: keyEnd}, nil
	case b == '\r' || b == '\n':
		return key{kind: keyEnter}, nil
	case b == 0x7f || b == '\b':
		return key{kind: keyBackspace}, nil
	case b == 0x1b:
		return s.readEscape()
	case b < 0x20:
		return key{kind: keyNone}, nil
	case b < 0x80:
		return key{kind: keyRune, r: rune(b)}, nil
	}

	need := utf8Len(b)
	if need == 0 {
		return key{kind: keyRune, r: '?'}, nil
	}
	buf := []byte{b}
	for len(buf) < need {
		nb, err := s.readByte()
		if err != nil {
			return key{kind: keyEOF}, err
		}
		buf = append(buf, nb)
	}
	r, _ := utf8.DecodeRune(buf)
	if r == utf8.RuneError {
		r = '?'
	}
	return key{kind: keyRune, r: r}, nil
}

func (s *Screen) readEscape() (key, error) {
	b, err := s.readByte()
	if err != nil {
		return key{kind: keyEOF}, err
	}
	if b == 0x09 {
		// ESC+Tab：部分终端/键盘把 Shift+Tab 编码成"Meta+Tab"（ESC 前缀）
		return key{kind: keyBackTab}, nil
	}
	if b != '[' && b != 'O' {
		return key{kind: keyNone}, nil
	}
	var params []byte
	for i := 0; i < 16; i++ {
		nb, err := s.readByte()
		if err != nil {
			return key{kind: keyEOF}, err
		}
		if nb >= 0x40 && nb <= 0x7E {
			return escKey(nb, params), nil
		}
		params = append(params, nb)
	}
	return key{kind: keyNone}, nil
}

func escKey(final byte, params []byte) key {
	switch final {
	case 'A':
		switch string(params) {
		case "1;2": // Shift+↑
			return key{kind: keyScrollUp}
		case "": // 裸 ↑：历史
			return key{kind: keyUp}
		}
	case 'B':
		switch string(params) {
		case "1;2": // Shift+↓
			return key{kind: keyScrollDown}
		case "": // 裸 ↓：历史
			return key{kind: keyDown}
		}
	case 'D':
		return key{kind: keyLeft}
	case 'C':
		return key{kind: keyRight}
	case 'Z': // Shift+Tab
		return key{kind: keyBackTab}
	case 'H':
		return key{kind: keyHome}
	case 'F':
		return key{kind: keyEnd}
	case '~':
		switch string(params) {
		case "1", "7":
			return key{kind: keyHome}
		case "3":
			return key{kind: keyDelete}
		case "4", "8":
			return key{kind: keyEnd}
		case "5":
			return key{kind: keyPageUp}
		case "6":
			return key{kind: keyPageDown}
		}
	}
	return key{kind: keyNone}
}

// maxPendingKeys 是流式输出期间缓存按键的上限（超出丢弃，避免无界增长）。
const maxPendingKeys = 64

// startKeyReader 启动后台读取 goroutine：它只从输入流读按键并送入通道，不触碰
// Screen 状态，因此 Screen 仍是"只被主 goroutine 修改"的单线程模型。
func (s *Screen) startKeyReader() {
	if s.keys != nil {
		return
	}
	s.keys = make(chan key, 64)
	go func() {
		defer close(s.keys)
		for {
			k, err := s.readKeyDirect()
			if err != nil {
				return
			}
			s.keys <- k
		}
	}()
}

// nextKey 取下一个按键：先取流式期间缓存的键，再等读取通道；测试中没有后台
// goroutine（keys 为 nil）时退化为直接阻塞读，保持原有测试路径。
func (s *Screen) nextKey() (key, error) {
	if n := len(s.pending); n > 0 {
		k := s.pending[0]
		s.pending = s.pending[1:]
		return k, nil
	}
	if s.keys != nil {
		k, ok := <-s.keys
		if !ok {
			return key{kind: keyEOF}, io.EOF
		}
		return k, nil
	}
	return s.readKeyDirect()
}

// pollInput 非阻塞取走已到达的按键：翻页键立刻生效，其余键缓存给下一次 ReadLine。
// 由 Screen.Write 在流式输出期间调用，"边输出边翻页"就是这么实现的。
func (s *Screen) pollInput() {
	if s.keys == nil || s.editing {
		return
	}
	for {
		select {
		case k, ok := <-s.keys:
			if !ok {
				return
			}
			if s.handleScrollKey(k) {
				continue
			}
			if len(s.pending) < maxPendingKeys {
				s.pending = append(s.pending, k)
			}
		default:
			return
		}
	}
}

// handleScrollKey 处理翻页键，返回是否已消费。
func (s *Screen) handleScrollKey(k key) bool {
	switch k.kind {
	case keyPageUp:
		s.scrollBack(s.rows - 1)
	case keyPageDown:
		s.scrollBack(-(s.rows - 1))
	case keyScrollUp:
		s.scrollBack(1)
	case keyScrollDown:
		s.scrollBack(-1)
	default:
		return false
	}
	return true
}

// SetCwd 设置路径补全的基准目录（相对路径按它解析）。
func (s *Screen) SetCwd(dir string) { s.cwd = dir }

// SetPrefill 设置下一次 ReadLine 的初始输入内容（用于跨模式携带已输入的文本）。
func (s *Screen) SetPrefill(txt string) { s.prefill = txt }

// histMax 是每种历史（AI / 命令）保留的条数上限。
const histMax = 100

// SetHistoryKind 选择 ↑↓ 浏览哪一份历史（"ai" / "cmd"）。
func (s *Screen) SetHistoryKind(kind string) { s.histKind = kind }

// AddHistory 记录一条已提交的输入到指定分组（"ai" / "cmd"；连续重复不记、超上限丢最旧）。
func (s *Screen) AddHistory(kind, line string) {
	line = strings.TrimSpace(line)
	if line == "" || kind == "" {
		return
	}
	if s.inputHist == nil {
		s.inputHist = map[string][]string{}
	}
	list := s.inputHist[kind]
	if n := len(list); n > 0 && list[n-1] == line {
		return
	}
	list = append(list, line)
	if len(list) > histMax {
		list = list[len(list)-histMax:]
	}
	s.inputHist[kind] = list
}

// histMove 在历史里上下移动（dir=+1 向更早，-1 向更新）；到底后恢复浏览前的草稿。
func (s *Screen) histMove(ed *lineEditor, dir int) {
	list := s.inputHist[s.histKind]
	if len(list) == 0 {
		return
	}
	if dir > 0 {
		switch {
		case s.histPos == -1:
			s.histDraft = string(ed.buf)
			s.histPos = len(list) - 1
		case s.histPos > 0:
			s.histPos--
		default:
			return // 已在最旧一条
		}
	} else {
		if s.histPos == -1 {
			return
		}
		s.histPos++
		if s.histPos >= len(list) { // 越过最新一条：回到草稿
			s.histPos = -1
			ed.setLine(s.histDraft)
			return
		}
	}
	ed.setLine(list[s.histPos])
}

// setLine 用给定内容替换输入行（光标落到行尾）。
func (ed *lineEditor) setLine(txt string) {
	ed.buf = []rune(txt)
	ed.cur = len(ed.buf)
	ed.comp = nil
	ed.cands = nil
	ed.redraw()
}

// ConsumeTabToggle 返回最近一次 ReadLine 中是否按下了 Tab（并清掉标志）。
func (s *Screen) ConsumeTabToggle() bool {
	t := s.tabToggle
	s.tabToggle = false
	return t
}

// ReadLine 显示 prompt 并进入行编辑（需已切 raw 模式），返回回车确认的一行。
// 返回 io.EOF 表示输入结束（Ctrl-D 或输入流关闭）。
func (s *Screen) ReadLine(prompt string) (string, error) {
	s.editing = true
	defer func() { s.editing = false }()
	ed := &lineEditor{s: s, prompt: []rune(prompt)}
	s.histPos, s.histDraft = -1, ""
	if s.prefill != "" {
		ed.buf = []rune(s.prefill)
		ed.cur = len(ed.buf)
		s.prefill = ""
	}
	ed.redraw()

	for {
		k, err := s.nextKey()
		if err != nil {
			s.HideCursor()
			return "", err
		}
		// Shift+Tab：切换模式（输入行清空，内容交给下一次 ReadLine 作为初始输入）
		if k.kind == keyBackTab {
			line := string(ed.buf)
			ed.clearRow()
			s.HideCursor()
			s.tabToggle = true
			return line, nil
		}
		// Tab：命令补全（不动模式）
		if k.kind == keyTab {
			ed.complete()
			continue
		}
		// 回看（自绘屏幕）：翻页键只移动视图；按其它任何键都先回到实时视图
		if s.handleScrollKey(k) {
			continue
		}
		s.scrollToLive()
		// ↑↓ 浏览历史；其它键视为编辑，退出浏览态
		if k.kind == keyUp {
			s.histMove(ed, +1)
			continue
		}
		if k.kind == keyDown {
			s.histMove(ed, -1)
			continue
		}
		s.histPos, s.histDraft = -1, ""
		switch k.kind {
		case keyEnter:
			ed.imeEnter() // 组合态：先提交首选词再送行
			line := string(ed.buf)
			ed.clearRow()
			s.HideCursor()
			ed.echo() // 固定输入行不进会话流：显式回显到内容区
			return line, nil
		case keyToggleIME:
			ed.toggleIME()
		case keyEOF:
			if len(ed.buf) == 0 {
				ed.clearRow()
				s.HideCursor()
				s.Write([]byte("\n"))
				return "", io.EOF
			}
		case keyInterrupt:
			ed.clearRow()
			s.HideCursor()
			s.Write([]byte("^C\n"))
			return "", nil
		case keyBackspace:
			if !ed.imeBackspace() && ed.cur > 0 {
				ed.buf = append(ed.buf[:ed.cur-1], ed.buf[ed.cur:]...)
				ed.cur--
				ed.redraw()
			}
		case keyDelete:
			if ed.cur < len(ed.buf) {
				ed.buf = append(ed.buf[:ed.cur], ed.buf[ed.cur+1:]...)
				ed.redraw()
			}
		case keyLeft:
			if ed.cur > 0 {
				ed.cur--
				ed.redraw()
			}
		case keyRight:
			if ed.cur < len(ed.buf) {
				ed.cur++
				ed.redraw()
			}
		case keyHome:
			if ed.cur != 0 {
				ed.cur = 0
				ed.redraw()
			}
		case keyEnd:
			if ed.cur != len(ed.buf) {
				ed.cur = len(ed.buf)
				ed.redraw()
			}
		case keyClearLine:
			hadComp := ed.composing()
			ed.imeClear()
			if len(ed.buf) > 0 || hadComp {
				ed.buf = nil
				ed.cur = 0
				ed.redraw()
			}
		case keyRune:
			if ed.imeKey(k.r) {
				break
			}
			ed.buf = append(ed.buf, 0)
			copy(ed.buf[ed.cur+1:], ed.buf[ed.cur:])
			ed.buf[ed.cur] = k.r
			ed.cur++
			ed.redraw()
		}
	}
}

// clearRow 清空固定输入行（提交/切换模式/结束时调用）。
func (ed *lineEditor) clearRow() {
	s := ed.s
	if s.inputRow < 0 || s.inputRow >= s.gridRows() {
		return
	}
	ed.imeClear()
	b := s.blank()
	row := s.grid[s.inputRow*s.cols : (s.inputRow+1)*s.cols]
	for i := range row {
		row[i] = b
	}
	s.cursorVis = false
	s.inputCursor = false
	s.repaintInputRow()
}

// echo 把"提示符 + 输入"回显到内容区（固定输入行不会留在会话流里）。
func (ed *lineEditor) echo() {
	s := ed.s
	s.fg, s.dim = s.promptColor(), false
	s.Write([]byte(string(ed.prompt) + string(ed.buf) + "\n"))
	s.fg, s.dim = cDefault, false
}

// promptColor 提示符颜色：命令模式绿色、AI 模式青色。
func (s *Screen) promptColor() sgrColor {
	if s.histKind == "cmd" {
		return cGreen
	}
	return cCyan
}

// lineEditor 维护固定输入行上的“prompt + 输入”；每次修改整行重绘。
type lineEditor struct {
	s      *Screen
	prompt []rune
	buf    []rune
	cur    int

	// 补全：上一次 Tab 的候选（>1）与当时的整行内容——整行没变时再按一次 Tab 才列出。
	lastComp   []string
	lastCompLn string

	// 输入法组合态（仅 IME 开启时非空；见 ime.go）
	comp  []rune   // 拼音串
	cands []string // 候选（≤9）
}

// redraw 把"提示符 + 输入 + 输入法后缀"渲染到固定输入行。
// 内容超宽时左移窗口（skip）以保住光标可见；越界部分由 putRune 裁掉。
// 绘制期间临时借用 curX/curY 写网格，结束时恢复内容光标——输入行有自己的光标
// （inputCurX），内容流的位置不能被它带偏。
func (ed *lineEditor) redraw() {
	s := ed.s
	if s.inputRow < 0 || s.inputRow >= s.gridRows() {
		return
	}
	savedX, savedY := s.curX, s.curY
	promptW, bufW := 0, 0
	for _, r := range ed.prompt {
		promptW += s.font.Width(r)
	}
	for _, r := range ed.buf {
		bufW += s.font.Width(r)
	}
	skip := 0
	if total := promptW + bufW; total >= s.cols {
		skip = total - s.cols + 1
	}

	b := s.blank()
	row := s.grid[s.inputRow*s.cols : (s.inputRow+1)*s.cols]
	for i := range row {
		row[i] = b
	}

	s.cursorVis = false
	s.inputCursor = false
	s.inputPaint = true
	s.setCursor(-skip, s.inputRow)
	s.fg, s.dim = s.promptColor(), false
	for _, r := range ed.prompt {
		s.putRune(r)
	}
	cursorX := s.curX // ed.cur == 0（光标在行首）时即提示符末尾
	s.fg, s.dim = cDefault, false
	for i, r := range ed.buf {
		s.putRune(r)
		if i+1 == ed.cur {
			cursorX = s.curX
		}
	}
	if ed.cur >= len(ed.buf) { // 光标在行尾：落在最后一个字符之后
		cursorX = s.curX
	}
	ed.drawIMESuffix() // 拼音/候选/[中]：沿用 putRune，超宽会被裁掉
	s.inputPaint = false
	s.setCursor(savedX, savedY) // 归还内容光标

	if cursorX < 0 {
		cursorX = 0
	}
	if cursorX >= s.cols {
		cursorX = s.cols - 1
	}
	s.inputCurX = cursorX
	s.inputCursor = true
	s.cursorVis = true
	s.paintRow(s.inputRow, row) // 整行刷一次：清掉上一帧残留（行变短/光标移动）
	s.repaintCursor()
}
