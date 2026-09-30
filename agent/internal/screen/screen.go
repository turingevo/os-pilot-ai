package screen

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	cellW = 8  // 窄字符宽（px）
	cellH = 16 // 行高（px）
)

const colBg uint32 = 0x000000

// 状态栏配色（与回看提示条同一色系）。
const (
	statusBg uint32 = 0x000060
	statusFg uint32 = 0xFFFFFF
)

// maxScrollback 是回看保留的历史行数上限（每行 cols 个 cell）。
const maxScrollback = 300

// statusRows 是底部保留的行数：固定输入行 + 状态栏（内容区 = 窗口行数 - 该值）。
const statusRows = 2

// SGR 颜色（agent 输出只用到 默认/红/绿/青）
type sgrColor int

const (
	cDefault sgrColor = iota
	cRed
	cGreen
	cCyan
)

func resolveFg(c sgrColor, dim bool) uint32 {
	if dim {
		switch c {
		case cRed:
			return 0xB04040
		case cGreen:
			return 0x40B040
		case cCyan:
			return 0x40B0B0
		default:
			return 0x9E9E9E
		}
	}
	switch c {
	case cRed:
		return 0xFF5555
	case cGreen:
		return 0x55FF55
	case cCyan:
		return 0x55FFFF
	default:
		return 0xFFFFFF
	}
}

type cell struct {
	r    rune   // ' ' = 空白
	wide bool   // 宽字符左半格
	cont bool   // 宽字符右半格（不独立绘制）
	fg   uint32 // 已解析的前景色
}

// VT 解析状态
const (
	stNormal = iota
	stEsc
	stCSI
	stUTF8
)

// Screen 是自绘屏幕。非并发安全：agent 的输入输出都在同一 goroutine。
type Screen struct {
	cv   Canvas
	font *Font
	cols int
	rows int
	grid []cell

	curX, curY int
	cursorVis  bool

	fg  sgrColor
	dim bool

	scrollN int // 累计滚动行数（行编辑器用它校准行起点）

	// 回看：已滚出屏幕的历史行（最旧在前）+ 视图相对实时底部向上偏移的行数（0=实时视图）
	hist    [][]cell
	viewOff int

	state  int
	csiBuf []byte
	ubuf   []byte
	uneed  int

	// 输入（open.go 填充；测试可替换 in）
	in   readerOnly
	tty  ttyFile
	orig termiosT

	// 输入（input.go）：后台读取 goroutine 把按键送入 keys；pending 缓存流式输出
	// 期间按下的非翻页键，交给下一次 ReadLine；editing 标记行编辑中（不做轮询）。
	// 三者都只在主 goroutine 上读写，Screen 的单线程模型不变。
	keys    chan key
	pending []key
	editing bool

	// 底部 UI：screenRows 是窗口总行数；rows 是内容区行数；inputRow 是固定输入行行号
	// （-1 = 没有固定输入行，输入行落在内容区里）。inputPaint 为真时 putRune 只写输入行、
	// 不换行也不滚动。
	//
	// 固定输入行有自己的光标（inputCurX/inputCursor）；curX/curY 始终只属于内容流。
	// 画输入行不改动内容光标，否则回车回显会从输入光标那一列开始写（整行偏到中间）。
	screenRows  int
	status      string
	inputRow    int
	inputPaint  bool
	inputCurX   int
	inputCursor bool

	// Tab 切换模式：行编辑中按 Tab 时置位，由调用方消费；prefill 是下次 ReadLine 的
	// 初始内容（跨模式携带用户已输入的文本）。
	tabToggle bool
	prefill   string

	// cwd 是路径补全的基准目录（命令模式下由 session 同步为 run_command 的工作目录）。
	cwd string

	// 输入历史（按 kind 分组，仅内存）：↑↓ 浏览；histPos = -1 表示未在浏览。
	// 注意与回看用的 hist（[][]cell）区分。
	inputHist map[string][]string
	histKind  string
	histPos   int
	histDraft string

	// 拼音输入法（ime.go；仅自绘屏幕路径使用）
	ime         imeEngine
	imeOn       bool   // 中/英状态（跨行保持）
	imeDead     bool   // 已判定不可用/失效，不再重试
	imeNotice   string // 一次性提示，绘制一帧后清除
	imeSuppress bool   // 行结束重绘时跳过后缀绘制（见 endLine）

	closed bool
}

func newScreen(cv Canvas, font *Font) *Screen {
	w, h := cv.Size()
	total := h / cellH
	s := &Screen{cv: cv, font: font, cols: w / cellW, screenRows: total, rows: total, inputRow: -1}
	s.grid = make([]cell, s.cols*s.rows)
	s.clearScreen()
	return s
}

// SetStatusRows 在底部保留 n 行（须在写入内容之前调用：网格按新尺寸重建）。
// n>=2 时：倒数第二行是固定输入行、最后一行是状态栏；内容区 = screenRows - n。
// 不调用时内容区占满整屏（文本控制台路径与大部分测试如此）。
func (s *Screen) SetStatusRows(n int) {
	if n <= 0 || n >= s.screenRows {
		return
	}
	s.rows = s.screenRows - n
	rows := s.rows
	if n >= 2 {
		rows++ // 固定输入行也放进网格，渲染可复用 putRune
		s.inputRow = s.rows
	}
	s.grid = make([]cell, s.cols*rows)
	s.hist = nil
	s.clearScreen()
}

// gridRows 返回网格总行数（内容区 + 可选固定输入行）。
func (s *Screen) gridRows() int {
	if s.cols == 0 {
		return 0
	}
	return len(s.grid) / s.cols
}

// repaintInputRow 按网格重绘固定输入行（含光标反白）；未设置输入行时忽略。
func (s *Screen) repaintInputRow() {
	if s.inputRow < 0 || s.inputRow >= s.gridRows() {
		return
	}
	s.paintRow(s.inputRow, s.grid[s.inputRow*s.cols:(s.inputRow+1)*s.cols])
	if s.cursorVis && s.inputCursor {
		s.repaintCell(s.inputCurX, s.inputRow)
	}
}

// repaintCursor 重绘光标所在单元（光标可能在固定输入行上）。
func (s *Screen) repaintCursor() {
	if s.inputRow >= 0 && s.inputCursor {
		s.repaintCell(s.inputCurX, s.inputRow)
		return
	}
	s.repaintCursorCell()
}

// SetStatus 设置状态栏文本（内容区未保留行时忽略）。
func (s *Screen) SetStatus(txt string) {
	if s.screenRows <= s.rows || s.status == txt {
		return
	}
	s.status = txt
	s.paintStatus()
}

// statusRowY 返回状态栏所在的窗口行号：永远是屏幕最后一行
// （固定输入行在它上面一行，故不能直接用 s.rows——那是输入行）。
func (s *Screen) statusRowY() int { return s.screenRows - 1 }

// paintStatus 在底部保留行重绘状态栏（属于 UI 层，回看冻结时也照画）。
func (s *Screen) paintStatus() {
	if s.screenRows <= s.rows {
		return
	}
	w, _ := s.cv.Size()
	y := s.statusRowY()
	s.cv.Fill(0, y*cellH, w, cellH, statusBg)
	x := 0
	for _, r := range s.status {
		gw := s.font.Width(r)
		if x+gw > s.cols {
			break
		}
		g, _ := s.font.Lookup(r)
		s.paintGlyph(x*cellW, y*cellH, g, gw*cellW, statusFg, statusBg)
		x += gw
	}
}

// Cols / Rows 返回字符网格尺寸（列以窄字符计）。
func (s *Screen) Cols() int { return s.cols }
func (s *Screen) Rows() int { return s.rows }

// ScrollCount 返回累计滚动行数。
func (s *Screen) ScrollCount() int { return s.scrollN }

func (s *Screen) idx(x, y int) int { return y*s.cols + x }

func (s *Screen) blank() cell { return cell{r: ' ', fg: resolveFg(cDefault, false)} }

func (s *Screen) clearScreen() {
	b := s.blank()
	for i := range s.grid {
		s.grid[i] = b
	}
	w, h := s.cv.Size()
	if s.viewOff == 0 { // 回看中冻结画面，回到实时视图时整屏重绘
		s.cv.Fill(0, 0, w, h, colBg)
		s.paintStatus()
		s.repaintInputRow()
	}
	s.curX, s.curY = 0, 0
}

// Write 渲染一段（含 UTF-8 与 ANSI 子集）字节流。实现 io.Writer。
func (s *Screen) Write(p []byte) (int, error) {
	s.pollInput() // 流式输出期间也能翻页；行编辑中由编辑器自己驱动，不轮询
	vis := s.cursorVis
	if vis {
		s.cursorVis = false
		s.repaintCursor()
	}
	for _, b := range p {
		s.feed(b)
	}
	if vis {
		s.cursorVis = true
		s.repaintCursor()
	}
	return len(p), nil
}

// ShowCursor / HideCursor 控制光标反白块。
func (s *Screen) ShowCursor() {
	if !s.cursorVis {
		s.cursorVis = true
		s.repaintCursor()
	}
}

func (s *Screen) HideCursor() {
	if s.cursorVis {
		s.cursorVis = false
		s.repaintCursor()
	}
}

// repaintCursorCell 重绘光标当前所在格（落在宽字符右半时归到左半）。
func (s *Screen) repaintCursorCell() {
	if s.curY < 0 || s.curY >= s.gridRows() || s.curX < 0 || s.curX >= s.cols {
		return // 行号越界（等待下次滚动落地）时不绘制
	}
	x, y := s.curX, s.curY
	if s.grid[s.idx(x, y)].cont {
		x--
	}
	s.repaintCell(x, y)
}

func (s *Screen) feed(b byte) {
	switch s.state {
	case stEsc:
		if b == '[' {
			s.state = stCSI
			s.csiBuf = s.csiBuf[:0]
			return
		}
		s.state = stNormal
		return
	case stCSI:
		switch {
		case b >= 0x40 && b <= 0x7E:
			s.csiFinal(b)
			s.state = stNormal
		case b >= 0x30 && b <= 0x3F:
			if len(s.csiBuf) < 32 {
				s.csiBuf = append(s.csiBuf, b)
			}
		}
		return
	case stUTF8:
		if b&0xC0 != 0x80 { // 非法续字节：放弃并重新处理
			s.state = stNormal
			s.putRune('?')
			s.feed(b)
			return
		}
		s.ubuf = append(s.ubuf, b)
		if len(s.ubuf) >= s.uneed {
			r, _ := utf8.DecodeRune(s.ubuf)
			s.ubuf = s.ubuf[:0]
			s.state = stNormal
			if r == utf8.RuneError {
				r = '?'
			}
			s.putRune(r)
		}
		return
	}

	switch {
	case b == 0x1b:
		s.state = stEsc
	case b == '\n': // 本渲染器中 \n 即“换行并回到行首”
		s.newLine()
	case b == '\r':
		s.curX = 0
	case b == '\t':
		for n := 8 - s.curX%8; n > 0; n-- {
			s.putRune(' ')
		}
	case b == '\b':
		if s.curX > 0 {
			s.curX--
		}
	case b < 0x20:
		// 其余控制字符忽略
	case b < 0x80:
		s.putRune(rune(b))
	default:
		need := utf8Len(b)
		if need == 0 {
			s.putRune('?')
			return
		}
		s.ubuf = append(s.ubuf[:0], b)
		s.uneed = need
		s.state = stUTF8
	}
}

func utf8Len(b byte) int {
	switch {
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	}
	return 0
}

func (s *Screen) csiFinal(b byte) {
	arg := 0
	if len(s.csiBuf) > 0 {
		arg, _ = strconv.Atoi(strings.TrimLeft(string(s.csiBuf), "?"))
	}
	switch b {
	case 'm':
		for _, p := range strings.Split(string(s.csiBuf), ";") {
			n, _ := strconv.Atoi(p)
			switch n {
			case 0:
				s.fg, s.dim = cDefault, false
			case 2:
				s.dim = true
			case 22:
				s.dim = false
			case 31:
				s.fg = cRed
			case 32:
				s.fg = cGreen
			case 36:
				s.fg = cCyan
			case 39:
				s.fg = cDefault
			}
		}
	case 'K':
		s.eraseLine(arg)
	case 'J':
		s.eraseDisplay(arg)
	}
}

// putRune 在当前光标处绘制字符并前进（含行尾换行与宽字符占格处理）。
func (s *Screen) putRune(r rune) {
	g, _ := s.font.Lookup(r)
	w := 1
	if g.Wide {
		w = 2
	}
	if s.inputPaint {
		if s.curX < 0 || s.curX+w > s.cols { // 固定输入行：超出左右边界直接裁掉
			s.curX += w
			return
		}
	} else if s.curX+w > s.cols {
		s.newLine()
	}
	s.ensureRow()
	// 覆盖旧宽字符时清掉其另一侧
	if c := &s.grid[s.idx(s.curX, s.curY)]; c.cont {
		s.clearCell(s.curX-1, s.curY)
		s.clearCell(s.curX, s.curY)
	}
	if c := &s.grid[s.idx(s.curX, s.curY)]; c.wide {
		s.clearCell(s.curX+1, s.curY)
	}

	fg := resolveFg(s.fg, s.dim)
	if g.Wide {
		s.grid[s.idx(s.curX, s.curY)] = cell{r: r, wide: true, fg: fg}
		s.grid[s.idx(s.curX+1, s.curY)] = cell{cont: true}
		s.repaintCell(s.curX, s.curY)
		s.curX += 2
	} else {
		s.grid[s.idx(s.curX, s.curY)] = cell{r: r, fg: fg}
		s.repaintCell(s.curX, s.curY)
		s.curX++
	}
	if !s.inputPaint && s.curX >= s.cols {
		s.curX = 0
		s.curY++ // 滚动延迟到真正写下一字符时（终端语义）
	}
}

func (s *Screen) newLine() {
	s.curX = 0
	s.curY++
}

// ensureRow 把可能越界的行号落回屏幕（必要时连续滚动）；固定输入行不滚动。
func (s *Screen) ensureRow() {
	if s.inputPaint {
		return
	}
	for s.curY >= s.rows {
		s.scroll()
		s.curY--
	}
}

func (s *Screen) scroll() {
	s.pushHist()
	if s.viewOff > 0 {
		// 回看中：画面冻结在旧内容上，只更新实时缓冲；偏移随内容一起增长，视图保持稳定。
		s.shiftContent()
		s.scrollN++
		s.viewOff++
		if s.viewOff > len(s.hist) { // 历史被裁掉导致偏移越界：贴到最旧一行
			s.viewOff = len(s.hist)
			s.repaintFull()
		}
		return
	}
	// 只滚内容区像素（固定输入行与状态栏不受影响）
	s.cv.ScrollUpRegion(0, s.rows*cellH, cellH)
	s.shiftContent()
	s.scrollN++
}

// shiftContent 把内容区整体上移一行（不含固定输入行），末行清空。
func (s *Screen) shiftContent() {
	n := s.rows * s.cols
	if s.inputRow >= 0 { // 网格多出一行（固定输入行）不参与内容滚动
		copy(s.grid[:n], s.grid[s.cols:n+s.cols])
	} else { // 没有输入行时网格就是内容区，末行滚出即丢弃
		copy(s.grid, s.grid[s.cols:])
	}
	s.blankLastRow()
}

// pushHist 把当前顶行记入历史（超出上限丢弃最旧的）。
func (s *Screen) pushHist() {
	row := make([]cell, s.cols)
	copy(row, s.grid[:s.cols])
	s.hist = append(s.hist, row)
	if len(s.hist) > maxScrollback {
		s.hist = s.hist[len(s.hist)-maxScrollback:]
	}
}

// blankLastRow 清空内容区最后一行（不含固定输入行）。
func (s *Screen) blankLastRow() {
	last := s.grid[(s.rows-1)*s.cols : s.rows*s.cols]
	b := s.blank()
	for i := range last {
		last[i] = b
	}
}

// scrollBack 调整回看偏移（正=向更早的行），必要时整屏重绘。
func (s *Screen) scrollBack(delta int) {
	if len(s.hist) == 0 {
		return // 没有历史可回看
	}
	off := s.viewOff + delta
	if off < 0 {
		off = 0
	}
	if off > len(s.hist) {
		off = len(s.hist)
	}
	if off == s.viewOff {
		return
	}
	s.viewOff = off
	s.repaintFull()
}

// scrollToLive 回到实时视图（回看中按下其它键时调用）。
func (s *Screen) scrollToLive() {
	if s.viewOff == 0 {
		return
	}
	s.viewOff = 0
	s.repaintFull()
}

// ViewOffset 返回当前回看偏移行数（0=实时视图）。
func (s *Screen) ViewOffset() int { return s.viewOff }

// repaintFull 按当前视图偏移整屏重绘：历史行 + 实时网格。
func (s *Screen) repaintFull() {
	w, h := s.cv.Size()
	s.cv.Fill(0, 0, w, h, colBg)
	for y := 0; y < s.rows; y++ {
		if row := s.visibleRowCells(y); row != nil {
			s.paintRow(y, row)
		}
	}
	if s.viewOff == 0 {
		s.repaintCursorCell()
	} else {
		s.drawScrollIndicator()
	}
	s.paintStatus()
	s.repaintInputRow()
}

// visibleRowCells 返回当前视图第 y 行对应的单元（回看时来自历史行）。
func (s *Screen) visibleRowCells(y int) []cell {
	if y < 0 || y >= s.rows {
		return nil
	}
	total := len(s.hist) + s.rows
	idx := total - s.rows - s.viewOff + y
	if idx < 0 || idx >= total {
		return nil
	}
	if idx < len(s.hist) {
		return s.hist[idx]
	}
	g := (idx - len(s.hist)) * s.cols
	return s.grid[g : g+s.cols]
}

// paintRow 把一行单元画到第 y 行（不写网格，供回看重绘）。
func (s *Screen) paintRow(y int, row []cell) {
	for x := 0; x < s.cols && x < len(row); x++ {
		c := row[x]
		if c.cont {
			continue
		}
		var g Glyph
		if c.r != 0 {
			g, _ = s.font.Lookup(c.r)
		}
		wpx := cellW
		if c.wide {
			wpx = 2 * cellW
		}
		s.paintGlyph(x*cellW, y*cellH, g, wpx, c.fg, colBg)
	}
}

// drawScrollIndicator 回看时在首行右上角显示偏移提示（回到实时视图后自然消失）。
func (s *Screen) drawScrollIndicator() {
	txt := fmt.Sprintf(" [回看 ↑%d 行] ", s.viewOff)
	width := 0
	for _, r := range txt {
		width += s.font.Width(r)
	}
	x := s.cols - width
	if x < 0 {
		x = 0
	}
	for _, r := range txt {
		if x >= s.cols {
			break
		}
		g, _ := s.font.Lookup(r)
		w := s.font.Width(r)
		if x+w <= s.cols {
			s.paintGlyph(x*cellW, 0, g, w*cellW, 0x55FFFF, 0x000060)
		}
		x += w
	}
}

func (s *Screen) clearCell(x, y int) {
	if x < 0 || x >= s.cols || y < 0 || y >= s.gridRows() {
		return
	}
	s.grid[s.idx(x, y)] = s.blank()
	s.repaintCell(x, y)
}

// repaintCell 按网格内容重绘一个单元（宽字符连同右半格；光标所在格反白）。
func (s *Screen) repaintCell(x, y int) {
	if s.inputPaint {
		return // 固定输入行由 redraw 整行刷一次，避免逐格刷漏掉被清空的旧像素
	}
	if x < 0 || x >= s.cols || y < 0 || y >= s.gridRows() {
		return
	}
	if s.viewOff != 0 {
		return // 回看中画面冻结，不画实时内容
	}
	c := &s.grid[s.idx(x, y)]
	if c.cont {
		return
	}
	var g Glyph
	if c.r != 0 {
		g, _ = s.font.Lookup(c.r)
	}
	wpx := cellW
	if c.wide {
		wpx = 2 * cellW
	}
	// 光标反白：固定输入行用自己的光标列，内容区用内容光标。
	curX, curY := s.curX, s.curY
	if s.inputRow >= 0 && s.inputCursor {
		curX, curY = s.inputCurX, s.inputRow
	}
	inv := s.cursorVis && curY == y && (curX == x || (c.wide && curX == x+1))
	fgc, bgc := c.fg, colBg
	if inv {
		fgc, bgc = bgc, c.fg
	}
	s.paintGlyph(x*cellW, y*cellH, g, wpx, fgc, bgc)
}

// paintGlyph 把一个字形位图刷到像素位置（fg 画点、bg 填底）。
func (s *Screen) paintGlyph(px0, py0 int, g Glyph, wpx int, fgc, bgc uint32) {
	for row := 0; row < cellH; row++ {
		bits := uint16(g.Bits[row*2])<<8 | uint16(g.Bits[row*2+1])
		for col := 0; col < wpx; col++ {
			cl := bgc
			if bits&(0x8000>>uint(col)) != 0 {
				cl = fgc
			}
			s.cv.SetPixel(px0+col, py0+row, cl)
		}
	}
}

// setCursor 只改坐标（不清旧像素，调用方自行重绘）。
func (s *Screen) setCursor(x, y int) {
	s.curX, s.curY = x, y
}

// eraseToEOS 从当前光标格清到内容区末尾（不动固定输入行/状态栏）。
func (s *Screen) eraseToEOS() {
	if s.curY >= s.rows {
		return // 光标在屏外（等待滚动）
	}
	end := s.rows * s.cols
	for i := s.idx(s.curX, s.curY); i < end; i++ {
		s.grid[i] = s.blank()
	}
	px := s.curX * cellW
	w, _ := s.cv.Size()
	if s.viewOff == 0 { // 回看中冻结画面
		s.cv.Fill(px, s.curY*cellH, w-px, cellH, colBg)
		if s.curY+1 < s.rows {
			s.cv.Fill(0, (s.curY+1)*cellH, w, (s.rows-s.curY-1)*cellH, colBg)
		}
	}
}

// eraseLine 处理 CSI K：0=光标到行尾，1=行首到光标，2=整行。
func (s *Screen) eraseLine(mode int) {
	if s.curY >= s.rows {
		return
	}
	switch mode {
	case 1:
		for x := 0; x <= s.curX && x < s.cols; x++ {
			s.clearCell(x, s.curY)
		}
	case 2:
		for x := 0; x < s.cols; x++ {
			s.clearCell(x, s.curY)
		}
	default:
		for x := s.curX; x < s.cols; x++ {
			s.clearCell(x, s.curY)
		}
	}
}

// eraseDisplay 处理 CSI J：0=光标到屏幕末尾，1=屏幕开头到光标，2=全屏。
func (s *Screen) eraseDisplay(mode int) {
	switch mode {
	case 1:
		for i := 0; i <= s.idx(s.curX, s.curY); i++ {
			s.grid[i] = s.blank()
		}
		if s.viewOff == 0 { // 回看中冻结画面
			s.cv.Fill(0, 0, s.cols*cellW, (s.curY+1)*cellH, colBg)
		}
		s.repaintCell(s.curX, s.curY)
	case 2:
		s.clearScreen()
	default:
		s.eraseToEOS()
	}
}

// textRow 返回一行的可见文本（宽字符占位按网格原样拼接），供测试与调试。
func (s *Screen) textRow(y int) string {
	if y < 0 || y >= s.rows {
		return ""
	}
	return rowText(s.grid[s.idx(0, y) : s.idx(0, y)+s.cols])
}

// visibleRow 返回当前视图第 y 行的文本（回看时来自历史行），供测试与调试。
func (s *Screen) visibleRow(y int) string { return rowText(s.visibleRowCells(y)) }

// inputRowText 返回固定输入行的文本，供测试与调试。
func (s *Screen) inputRowText() string {
	if s.inputRow < 0 || s.inputRow >= s.gridRows() {
		return ""
	}
	return rowText(s.grid[s.inputRow*s.cols : (s.inputRow+1)*s.cols])
}

func rowText(row []cell) string {
	var b strings.Builder
	for _, c := range row {
		switch {
		case c.cont:
			// 宽字符右半格：跳过
		case c.r == 0:
			b.WriteRune(' ')
		default:
			b.WriteRune(c.r)
		}
	}
	return b.String()
}
