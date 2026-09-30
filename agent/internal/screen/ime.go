package screen

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 本地控制台拼音输入法：helper 二进制（ime/pinyin-ime，libgooglepinyin 静态封装）
// 通过 stdio 协议提供候选；只在自绘屏幕路径使用，串口/文本控制台不涉及。

const (
	imeBinDefault  = "/ventoy/ai/ime/pinyin-ime"
	imeDictDefault = "/ventoy/ai/ime/dict_pinyin.dat"
	imeStartWait   = time.Second            // 启动握手超时（含词典加载）
	imeReqWait     = 500 * time.Millisecond // 单次检索/提交超时
)

// imeEngine 是行编辑器依赖的输入法引擎（便于单测注入 fake）。
type imeEngine interface {
	Search(pinyin string) ([]string, error) // 候选（≤9）
	Choose(idx int)                         // 提交候选（含引擎侧选字学习），失败静默
	Reset()                                 // 清空引擎侧组合状态
	Close()
}

// IMEAvailable 报告输入法组件是否可用（只探测文件，不启动进程）。
// 供启动横幅决定是否提示切换键。
func IMEAvailable() bool {
	if os.Getenv("VTOY_AI_IME") == "0" {
		return false
	}
	bin, dict := imePaths()
	if _, err := os.Stat(bin); err != nil {
		return false
	}
	if _, err := os.Stat(dict); err != nil {
		return false
	}
	return true
}

func imePaths() (bin, dict string) {
	bin = os.Getenv("VTOY_AI_IME_BIN")
	if bin == "" {
		bin = imeBinDefault
	}
	dict = os.Getenv("VTOY_AI_IME_DICT")
	if dict == "" {
		dict = imeDictDefault
	}
	return bin, dict
}

type imeClient struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	dead bool
}

// openIME 启动 helper 并完成握手（首次 Ctrl-Space 时懒调用）。
func openIME() (*imeClient, error) {
	if os.Getenv("VTOY_AI_IME") == "0" {
		return nil, errors.New("已通过 VTOY_AI_IME=0 禁用")
	}
	bin, dict := imePaths()
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("未内置输入法程序 %s", bin)
	}
	if _, err := os.Stat(dict); err != nil {
		return nil, fmt.Errorf("词典缺失 %s", dict)
	}
	user := os.Getenv("VTOY_AI_IME_USERDICT")
	if user == "" {
		user = filepath.Join(os.TempDir(), "pinyin-user.dat")
	}

	cmd := exec.Command(bin, dict, user)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, err
	}
	c := &imeClient{cmd: cmd, in: in, out: bufio.NewReader(out)}
	line, err := c.readLine(imeStartWait)
	if err != nil {
		c.kill()
		return nil, fmt.Errorf("输入法启动失败: %w", err)
	}
	if line != "R\tready" {
		c.kill()
		return nil, fmt.Errorf("输入法启动异常: %s", line)
	}
	return c, nil
}

func (c *imeClient) readLine(timeout time.Duration) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		ln, err := c.out.ReadString('\n')
		ch <- result{ln, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", r.err
		}
		return strings.TrimRight(r.line, "\r\n"), nil
	case <-time.After(timeout):
		return "", errors.New("输入法响应超时")
	}
}

func (c *imeClient) request(req string) (string, error) {
	if c.dead {
		return "", errors.New("输入法已不可用")
	}
	if _, err := io.WriteString(c.in, req+"\n"); err != nil {
		c.kill()
		return "", err
	}
	line, err := c.readLine(imeReqWait)
	if err != nil {
		c.kill()
		return "", err
	}
	return line, nil
}

// kill 终止 helper（幂等）；之后所有请求立即失败。
func (c *imeClient) kill() {
	if c.cmd == nil {
		return
	}
	c.dead = true
	_ = c.cmd.Process.Kill()
	_ = c.in.Close()
	cmd := c.cmd
	go func() { _ = cmd.Wait() }()
}

func (c *imeClient) Search(pinyin string) ([]string, error) {
	line, err := c.request("S\t" + pinyin)
	if err != nil {
		return nil, err
	}
	f := strings.Split(line, "\t")
	if len(f) < 2 || f[0] != "C" {
		return nil, fmt.Errorf("输入法协议错误: %q", line)
	}
	n, err := strconv.Atoi(f[1])
	if err != nil || n < 0 || n > len(f)-2 {
		n = len(f) - 2
	}
	return f[2 : 2+n], nil
}

func (c *imeClient) Choose(idx int) {
	if _, err := c.request(fmt.Sprintf("A\t%d", idx)); err != nil {
		return // 引擎失效不影响已上屏文本
	}
}

func (c *imeClient) Reset() {
	_, _ = c.request("R")
}

// Close 优雅结束 helper（先 Q 后强杀兜底），幂等。
func (c *imeClient) Close() {
	if c == nil || c.cmd == nil {
		return
	}
	if !c.dead {
		_, _ = io.WriteString(c.in, "Q\n")
		_ = c.in.Close()
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(imeReqWait):
			_ = c.cmd.Process.Kill()
			<-done
		}
	}
	c.cmd = nil
}

// ---------- 行编辑器侧：输入法状态机与后缀渲染 ----------

// composing 报告当前是否处于拼音组合中。
func (ed *lineEditor) composing() bool { return len(ed.comp) > 0 }

// toggleIME 处理 Ctrl-Space：开关输入法；首次开启懒启动 helper。
func (ed *lineEditor) toggleIME() {
	s := ed.s
	if s.imeDead {
		s.imeNotice = "输入法不可用"
		ed.redraw()
		return
	}
	if s.ime == nil {
		eng, err := openIME()
		if err != nil {
			s.imeDead = true
			s.imeNotice = "输入法不可用: " + imeErrText(err)
			ed.redraw()
			return
		}
		s.ime = eng
	}
	s.imeOn = !s.imeOn
	ed.imeClear()
	ed.redraw()
}

// imeKey 处理一个输入字符；返回 true 表示已消费（不走普通插入）。
func (ed *lineEditor) imeKey(r rune) bool {
	if !ed.s.imeOn {
		return false
	}
	if r >= 'A' && r <= 'Z' { // 大写直通，便于输入英文/路径
		return false
	}
	switch {
	case r >= 'a' && r <= 'z' || r == '\'':
		ed.comp = append(ed.comp, r)
		ed.imeRefresh()
		return true
	case r >= '1' && r <= '9':
		if !ed.composing() {
			return false
		}
		if idx := int(r - '1'); idx < len(ed.cands) {
			ed.imeCommit(idx)
		}
		return true // 组合中的越界数字吞掉，避免混入中文
	case r == ' ':
		if !ed.composing() {
			return false
		}
		if len(ed.cands) > 0 {
			ed.imeCommit(0)
		} else {
			ed.imeCommitRaw()
		}
		return true
	}
	return false
}

// imeRefresh 重新检索候选；引擎失效时降级关闭输入法。
func (ed *lineEditor) imeRefresh() {
	s := ed.s
	cands, err := s.ime.Search(string(ed.comp))
	if err != nil {
		s.imeDead = true
		s.imeOn = false
		s.imeNotice = "输入法失效: " + imeErrText(err)
		ed.comp, ed.cands = nil, nil
		ed.redraw()
		return
	}
	ed.cands = cands
	ed.redraw()
}

// imeCommit 提交第 idx 个候选（含引擎选字学习）。
func (ed *lineEditor) imeCommit(idx int) {
	if idx < 0 || idx >= len(ed.cands) {
		return
	}
	text := ed.cands[idx]
	ed.s.ime.Choose(idx)
	ed.comp, ed.cands = nil, nil
	ed.insertText(text)
	ed.redraw()
}

// imeCommitRaw 无候选时把拼音原样上屏（主流输入法行为）。
func (ed *lineEditor) imeCommitRaw() {
	if !ed.composing() {
		return
	}
	text := string(ed.comp)
	ed.s.ime.Reset()
	ed.comp, ed.cands = nil, nil
	ed.insertText(text)
	ed.redraw()
}

// imeBackspace 组合态退格：拼音退一格；返回 true 表示已消费。
func (ed *lineEditor) imeBackspace() bool {
	if !ed.s.imeOn || !ed.composing() {
		return false
	}
	ed.comp = ed.comp[:len(ed.comp)-1]
	if len(ed.comp) == 0 {
		ed.cands = nil
		ed.s.ime.Reset()
	} else {
		ed.imeRefresh()
		return true
	}
	ed.redraw()
	return true
}

// imeEnter 回车时先把组合提交为首选词（无候选则原样上屏）。
func (ed *lineEditor) imeEnter() {
	if !ed.s.imeOn || !ed.composing() {
		return
	}
	if len(ed.cands) > 0 {
		ed.imeCommit(0)
	} else {
		ed.imeCommitRaw()
	}
}

// imeClear 丢弃组合（Ctrl-U / 关闭输入法），不提交上屏。
func (ed *lineEditor) imeClear() {
	if ed.composing() && ed.s.ime != nil {
		ed.s.ime.Reset()
	}
	ed.comp, ed.cands = nil, nil
}

// endLine 行结束时清掉组合与输入法后缀（候选条/[中]/提示）并重绘，
// 避免后缀残留在已提交的历史行上（回车/Ctrl-C/Ctrl-D 路径）。
func (ed *lineEditor) endLine() {
	ed.imeClear()
	s := ed.s
	if s.imeSuppress {
		return
	}
	s.imeSuppress = true
	ed.redraw()
	s.imeSuppress = false
}

// insertText 在光标处插入文本。
func (ed *lineEditor) insertText(text string) {
	for _, r := range text {
		ed.buf = append(ed.buf, 0)
		copy(ed.buf[ed.cur+1:], ed.buf[ed.cur:])
		ed.buf[ed.cur] = r
		ed.cur++
	}
}

// drawIMESuffix 在行内容之后绘制输入法后缀：
// 一次性提示 > 组合（灰色拼音 + 候选条） > 状态标记 [中]。
func (ed *lineEditor) drawIMESuffix() {
	s := ed.s
	if s.imeSuppress {
		return
	}
	if s.imeNotice != "" {
		ed.drawDimText(" [" + s.imeNotice + "]")
		s.imeNotice = ""
		return
	}
	if !s.imeOn {
		return
	}
	if ed.composing() {
		ed.drawDimText(" " + string(ed.comp))
		for i, c := range ed.cands {
			s.putRune(' ')
			ed.putText(fmt.Sprintf("%d.%s", i+1, c))
		}
		return
	}
	ed.drawDimText(" [中]")
}

func (ed *lineEditor) drawDimText(text string) {
	s := ed.s
	savedDim, savedFg := s.dim, s.fg
	s.dim = true
	ed.putText(text)
	s.dim, s.fg = savedDim, savedFg
}

func (ed *lineEditor) putText(text string) {
	for _, r := range text {
		ed.s.putRune(r)
	}
}

func imeErrText(err error) string {
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	r := []rune(s)
	if len(r) > 40 {
		s = string(r[:40]) + "…"
	}
	return s
}
