package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"os-pilot-ai/internal/config"
	"os-pilot-ai/internal/llm"
	"os-pilot-ai/internal/screen"
	"os-pilot-ai/internal/tools"
)

const (
	maxToolItersPerTurn = 20
	maxHistoryMessages  = 80
	historyKeepTail     = 60
)

type Logger struct {
	mu    sync.Mutex
	jsonl *os.File
	md    *os.File
	Path  string
}

func NewLogger(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	base := filepath.Join(dir, "session-"+time.Now().Format("20060102-150405"))
	jf, err := os.Create(base + ".jsonl")
	if err != nil {
		return nil, err
	}
	mf, err := os.Create(base + ".md")
	if err != nil {
		jf.Close()
		return nil, err
	}
	return &Logger{jsonl: jf, md: mf, Path: base}, nil
}

func (l *Logger) event(kind string, fields map[string]any) {
	if l == nil {
		return
	}
	rec := map[string]any{"ts": time.Now().Format(time.RFC3339), "type": kind}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jsonl.Write(append(b, '\n'))
}

func mdRole(role string) string {
	switch role {
	case "user":
		return "用户"
	case "assistant":
		return "AI"
	case "tool":
		return "工具结果"
	default:
		return role
	}
}

func (l *Logger) Header(info map[string]string) {
	if l == nil {
		return
	}
	l.event("session_start", map[string]any{"info": info})
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "# AI 装机助手会话\n\n")
	for _, k := range []string{"time", "provider", "model", "mode", "payload_dir", "log_dir",
		"local_inference", "boot_log", "local_llm"} {
		if v, ok := info[k]; ok {
			fmt.Fprintf(l.md, "- %s: %s\n", k, v)
		}
	}
}

func (l *Logger) Msg(role, text string) {
	l.event("message", map[string]any{"role": role, "text": text})
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "\n### %s\n\n%s\n", mdRole(role), text)
}

func (l *Logger) ToolCall(name, args string) {
	l.event("tool_call", map[string]any{"name": name, "args": json.RawMessage(args)})
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "\n#### 工具调用: %s\n\n```json\n%s\n```\n", name, args)
}

func (l *Logger) ToolResult(name, result, errMsg string) {
	f := map[string]any{"name": name, "result": result}
	if errMsg != "" {
		f["error"] = errMsg
	}
	l.event("tool_result", f)
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "\n#### 工具返回: %s\n\n```\n%s\n```\n", name, result)
}

func (l *Logger) Note(text string) {
	l.event("note", map[string]any{"text": text})
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "\n> %s\n", text)
}

// End 与 Header 的 session_start 成对：没有结束事件就分不出"用户正常退出"和
// "进程被杀/自己崩了"，真机上这两种的表现都是突然关机。
func (l *Logger) End(code int, reason string) {
	if l == nil {
		return
	}
	l.event("session_end", map[string]any{"rc": code, "reason": reason})
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.md, "\n> 会话结束：rc=%d %s（%s）\n",
		code, reason, time.Now().Format("2006-01-02 15:04:05"))
}

func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jsonl.Sync()
	l.md.Sync()
	l.jsonl.Close()
	l.md.Close()
}

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
)

type consoleIO struct {
	in         *bufio.Reader
	out        io.Writer
	scr        *screen.Screen // 非 nil 时接管本地显示器自绘（中文渲染）
	script     bool
	log        *Logger
	color      bool
	promptANSI string // 文本控制台路径的提示符颜色（自绘屏按模式自己上色，不用它）
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (c *consoleIO) paint(code, s string) string {
	if !c.color {
		return s
	}
	return code + s + ansiReset
}

func (c *consoleIO) Printf(format string, args ...any) {
	fmt.Fprintf(c.out, format, args...)
}

func (c *consoleIO) ScriptMode() bool { return c.script }

// ConsumeTabToggle 自绘屏上按下 Tab 时返回 true（并消费）；文本路径恒为 false。
func (c *consoleIO) ConsumeTabToggle() bool {
	return c.scr != nil && c.scr.ConsumeTabToggle()
}

// SetPrefill 把文本设为下一次输入行的初始内容（自绘屏专有；文本路径忽略）。
func (c *consoleIO) SetPrefill(txt string) {
	if c.scr != nil {
		c.scr.SetPrefill(txt)
	}
}

// SetHistoryKind 选择 ↑↓ 浏览哪份历史（"ai" / "cmd"）。
func (c *consoleIO) SetHistoryKind(kind string) {
	if c.scr != nil {
		c.scr.SetHistoryKind(kind)
	}
}

// AddHistory 记录一条已提交的输入到指定历史分组。
func (c *consoleIO) AddHistory(kind, line string) {
	if c.scr != nil {
		c.scr.AddHistory(kind, line)
	}
}

// SetCwd 同步路径补全的基准目录（自绘屏专有；文本路径忽略）。
func (c *consoleIO) SetCwd(dir string) {
	if c.scr != nil {
		c.scr.SetCwd(dir)
	}
}

func (c *consoleIO) readLine(prompt string) (string, error) {
	if c.scr != nil {
		return c.scr.ReadLine(prompt)
	}
	fmt.Fprint(c.out, c.paint(c.promptANSI, prompt))
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *consoleIO) Ask(prompt string) (string, error) {
	if c.script {
		c.Printf("[脚本模式] 提问: %s（无交互输入）\n", prompt)
		return "", tools.ErrNoInput
	}
	return c.readLine(prompt + ": ")
}

func (c *consoleIO) Confirm(prompt, detail string) (bool, error) {
	if detail != "" {
		c.Printf("\n%s\n", detail)
	}
	if c.script {
		c.Printf("[脚本模式] 自动批准: %s\n", prompt)
		c.log.Note("auto-approve: " + prompt)
		return true, nil
	}
	ans, err := c.readLine(prompt + " [y/N]: ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y", "yes", "是":
		return true, nil
	}
	c.Printf("（已取消）\n")
	return false, nil
}

func (c *consoleIO) TypedConfirm(prompt, detail, word string) (bool, error) {
	if detail != "" {
		c.Printf("\n%s\n", detail)
	}
	if c.script {
		c.Printf("[脚本模式] 自动批准: %s\n", prompt)
		c.log.Note("auto-approve(typed): " + prompt)
		return true, nil
	}
	ans, err := c.readLine(fmt.Sprintf("%s\n请输入 %s 确认: ", prompt, word))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(ans) == word {
		return true, nil
	}
	c.Printf("（输入不匹配，已取消）\n")
	return false, nil
}

// ---------- 流式渲染 ----------

type streamState struct {
	sawReasoning bool
	sawContent   bool
	inFence      bool
	phaseSeen    bool // 首 token 已到并已上报"生成中"
	lineBuf      strings.Builder
}

func (c *consoleIO) beginStream() *streamState { return &streamState{} }

func (c *consoleIO) streamReasoning(st *streamState, delta string) {
	if !st.sawReasoning {
		st.sawReasoning = true
		c.Printf("\n%s\n", c.paint(ansiDim, "（思考中）"))
	}
	fmt.Fprint(c.out, c.paint(ansiDim, delta))
}

func (c *consoleIO) streamContent(st *streamState, delta string) {
	if !st.sawContent && strings.TrimSpace(delta) == "" {
		return
	}
	if st.sawReasoning && !st.sawContent {
		c.Printf("\n")
	}
	if !st.sawContent {
		st.sawContent = true
		c.Printf("\n%s ", c.paint(ansiCyan+ansiBold, "AI>"))
	}
	st.lineBuf.WriteString(delta)
	c.flushStreamLines(st)
}

func (c *consoleIO) flushStreamLines(st *streamState) {
	for {
		s := st.lineBuf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return
		}
		line := s[:i]
		st.lineBuf.Reset()
		st.lineBuf.WriteString(s[i+1:])
		c.printStreamLine(st, line)
	}
}

func (c *consoleIO) printStreamLine(st *streamState, line string) {
	if strings.HasPrefix(strings.TrimSpace(line), "```") {
		st.inFence = !st.inFence
		c.Printf("%s\n", c.paint(ansiDim, line))
		return
	}
	if st.inFence {
		c.Printf("%s\n", c.paint(ansiDim, line))
		return
	}
	c.Printf("%s\n", line)
}

func (c *consoleIO) endStream(st *streamState) {
	rest := st.lineBuf.String()
	st.lineBuf.Reset()
	if rest != "" {
		c.printStreamLine(st, rest)
	}
	if st.sawContent || st.sawReasoning {
		c.Printf("\n")
	}
}

// ---------- 工具调用/结果格式化 ----------

const (
	maxResultLines      = 40
	maxResultLinesBrief = 12
	maxResultWidth      = 200
)

func (c *consoleIO) toolCallLine(name, argText string) string {
	head := c.paint(ansiCyan, "  → "+name)
	if s := summarizeArgs(name, argText); s != "" {
		return head + "  " + s
	}
	return head
}

func summarizeArgs(name, argText string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argText), &m); err != nil || len(m) == 0 {
		return ""
	}
	switch name {
	case "run_command":
		cmd, _ := m["cmd"].(string)
		line := cmd
		if arr, ok := m["args"].([]any); ok {
			for _, v := range arr {
				line += " " + fmt.Sprint(v)
			}
		}
		return "$ " + strings.TrimSpace(line)
	case "fs_read", "fs_write", "fs_delete", "fs_list":
		if p, ok := m["path"].(string); ok {
			return p
		}
	case "schedule_boot":
		var parts []string
		for _, k := range []string{"image", "template", "autosel", "timeout"} {
			if v, ok := m[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
		}
		return strings.Join(parts, " ")
	case "ask_user":
		if q, ok := m["question"].(string); ok {
			return q
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func (c *consoleIO) toolResult(result, errMsg string, dur time.Duration) {
	if errMsg != "" {
		c.Printf("%s\n", c.paint(ansiRed, "  ← 失败: "+errMsg))
		return
	}
	body := result
	var head string
	if strings.HasPrefix(result, "exit_code=") {
		if i := strings.IndexByte(result, '\n'); i >= 0 {
			body = result[i+1:]
		}
		code := strings.TrimPrefix(strings.SplitN(result, "\n", 2)[0], "exit_code=")
		style := ansiGreen
		if code != "0" {
			style = ansiRed
		}
		head = c.paint(style, "  ← exit "+code)
	} else {
		head = c.paint(ansiGreen, "  ← 完成")
	}
	c.Printf("%s\n", head+c.paint(ansiDim, fmt.Sprintf(" · %.2fs", dur.Seconds())))

	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return
	}
	lines := strings.Split(body, "\n")
	limit := maxResultLinesBrief
	if strings.HasPrefix(result, "exit_code=") {
		limit = maxResultLines
	}
	if len(lines) > limit {
		lines = append(lines[:limit:limit], fmt.Sprintf("…（省略 %d 行，完整内容见日志）", len(lines)-limit))
	}
	for _, ln := range lines {
		c.Printf("    %s\n", oneLine(ln, maxResultWidth))
	}
}

type Session struct {
	Cfg     *config.Config
	Client  *llm.Client
	Reg     *tools.Registry
	Ctx     *tools.Context
	Log     *Logger
	history []llm.Message
	io      *consoleIO
	script  bool

	cmdMode bool   // true=命令模式（输入直接执行本地命令）；false=AI 模式（输入发给模型）
	cwd     string // 命令模式的工作目录（cd 改它；run_command 与路径补全都用它）

	probeInHistory string // 已写入模型的 system_probe 原文，用于识别重复探测

	phase     string    // 当前阶段文本（就绪/请求模型/生成中/调用工具/回答完毕），显示在状态栏或单行提示
	turnStart time.Time // 本轮开始时刻，用于整轮耗时
}

func New(cfg *config.Config, client *llm.Client, reg *tools.Registry, script bool, noReboot bool) (*Session, error) {
	logger, err := NewLogger(cfg.LogDir)
	if err != nil {
		logger, err = NewLogger(filepath.Join(os.TempDir(), "ventoy-ai-logs"))
		if err != nil {
			return nil, fmt.Errorf("初始化日志失败: %w", err)
		}
	}
	cio := &consoleIO{in: bufio.NewReader(os.Stdin), out: os.Stdout, script: script, log: logger, promptANSI: ansiCyan}
	cio.color = !script && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)
	if scr, serr := screen.Open(); serr != nil {
		logger.Note("屏幕接管失败，继续使用文本控制台: " + serr.Error())
	} else if scr != nil {
		cio.scr = scr
		cio.out = scr
		cio.color = !script && os.Getenv("NO_COLOR") == ""
		logger.Note("已接管本地显示器自绘（/dev/fb0）")
	}
	s := &Session{Cfg: cfg, Client: client, Reg: reg, Log: logger, io: cio, script: script, cwd: "/"}
	s.Ctx = &tools.Context{PayloadDir: cfg.PayloadDir, Mode: cfg.Mode, NoReboot: noReboot, Cwd: "/", IO: cio}
	cio.SetCwd("/")
	s.history = []llm.Message{{Role: "system", Content: s.systemPrompt()}}
	header := map[string]string{
		"time":        time.Now().Format("2006-01-02 15:04:05"),
		"provider":    cfg.Provider,
		"model":       cfg.Model,
		"mode":        cfg.Mode,
		"payload_dir": cfg.PayloadDir,
		"log_dir":     cfg.LogDir,
	}
	if cfg.LocalInference {
		header["local_inference"] = "true"
	}
	if v := os.Getenv("VTOY_AI_BOOT_LOG"); v != "" {
		header["boot_log"] = filepath.Base(v) // 与 log_dir 同目录，只报名字即可定位
	}
	if v := os.Getenv("VTOY_AI_LOCAL_LLM_REASON"); v != "" {
		header["local_llm"] = v
	}
	logger.Header(header)
	return s, nil
}

func (s *Session) systemPrompt() string {
	rebootNote := "schedule_boot 写入配置成功后会触发重启，进入自动安装。"
	if s.Ctx != nil && s.Ctx.NoReboot {
		rebootNote = "当前为调试模式（no_reboot）：schedule_boot 只写配置不会真正重启。"
	}
	return fmt.Sprintf(`你是 AI 装机助手，运行在一个由 U 盘启动的临时 Linux 环境（initramfs）中，通过串口/控制台与用户对话，帮助用户完成系统安装的准备工作。

两阶段工作模型：
1) 编排阶段（你所在的阶段）：通过问答了解用户要安装的系统，探测 U 盘上的镜像与脚本，生成/修改 Ventoy 配置（ventoy.json 的 control 与 auto_install 字段），使下次开机自动进入无人值守安装。
2) 执行阶段：由 Ventoy 主程序按配置自动引导 ISO 并执行应答脚本，不需要你参与。

工作守则：
- 先探测再行动：不要编造文件路径或设备名。先调用 system_probe 获取系统与 U 盘内容，再用 fs_read 查看具体文件后再决定修改。
- 改分区/格式化/备份前必须先调用 list_disks 看清有哪些盘、哪些 protected=true（承载运行环境或已挂载，禁止改动）。只对用户明确选定的目标盘操作。
- 分区/格式化会清空数据：partition（整盘重建分区表）与 format（建文件系统，可建类型以 format 工具 fstype 的清单为准，由随包工具探测得出）会先展示将执行的命令，并要求用户逐字输入设备名确认（direct 模式除外）；用一两句话说明"要做什么、为什么"后再调用。
- 跨平台数据盘（Windows/macOS/Android 之间互访）用 exfat；NTFS/HFS+ 这类格式在 Linux 端能力不对等（HFS+ 只能读写挂载与分区，不能新建），不要凭印象向用户承诺"格成某某格式"，以工具声明的清单为准。
- 分区容量要给足：桌面版 Linux（Ubuntu Desktop 等）根分区建议 ≥25 GiB —— 安装器要在根分区里生成 initramfs，太小会因空间不足失败（装完没有 initrd、开机进不去）；磁盘不大时不要单独拆 /home，否则两边都紧张。UEFI 机器还要留约 512MiB 的 EFI 系统分区（fs 提示 vfat）。给出分区方案时把容量和理由一起说明。
- 备份流程：partition 建分区 → format 建文件系统 → 用 run_command 的 mount 把目标分区挂载到某个目录（需确认）→ backup 把数据分区内容同步过去 → 用 umount 卸载。backup 只允许写向非敏感盘。
- 磁盘标识：不要把手写的 /dev/vdX、/dev/sdX 设备名写进自动安装模板——换机器/换接口/重启后会变。选盘用 list_disks 报出的 serial（磁盘序列号），挂载用卷标（label）；整盘设备名只在调用 partition/format/gen_autoinstall 时才用。
- 生成 Ubuntu 自动安装应答脚本必须用 gen_autoinstall 工具（它按 curtin schema 确定性拼装 storage 段：disk 用 serial、partition 用 device=盘id、format 用 volume=分区id、mount 用 device=格式id + path）；不要用 fs_write 手写同类 YAML（手写会出现硬编码设备名与非法字段，如 type: fs / fstab_id）。
- 修改 ventoy.json 必须用 schedule_boot 工具（它会保证 JSON 结构正确、自动备份 .bak、展示 diff 并请求用户确认）；其他普通文件（如应答脚本）才用 fs_write。
- 任何写操作前，先用一两句话向用户解释"将要做什么、为什么"，然后再调用工具。
- 如果用户只是咨询，直接回答，不要调用写工具。
- 工具路径可以是绝对路径或相对 payload_dir 的相对路径，但只能访问 payload_dir 之内。
- 遵守安全模式限制：readonly 只读（改分区/格式化/备份会被拒绝）；orchestrate 写操作需用户确认；direct 直接执行。
- 用简洁的中文回答（除非用户使用其他语言）。一次只推进一小步；缺少关键信息（目标系统版本、分区方案、用户名等）时用 ask_user 询问。
- 配置完成后提醒用户：重要数据请提前备份，装机过程会清空目标磁盘。

运行环境信息：
- 安全模式: %s
- 数据分区（payload_dir，可读写）: %s
- 回复语言: %s
- %s`, s.Cfg.Mode, s.Cfg.PayloadDir, s.Cfg.Language, rebootNote)
}

func (s *Session) printBanner() {
	model := s.Cfg.Model
	if len(model) > 48 {
		model = model[:48] + "..."
	}
	tag := ""
	if s.Cfg.LocalInference {
		tag = "（本地）"
	}
	s.io.Printf("================ AI 装机助手 ================\n")
	s.io.Printf("模型: %s%s | 模式: %s | 数据分区: %s\n", model, tag, s.Cfg.Mode, s.Cfg.PayloadDir)
	s.io.Printf("日志: %s.md\n", s.Log.Path)
	s.io.Printf("输入问题开始对话；Tab 切命令模式（提示符 $），/help 查看命令，输入 exit 退出\n")
	if s.io.scr != nil && !s.io.script && screen.IMEAvailable() {
		s.io.Printf("输入法: 内置拼音（Ctrl-Space 切换中/英，空格/回车提交候选）\n")
	}
	if s.io.scr != nil && !s.io.script {
		s.io.Printf("回看: PageUp/PageDown 翻页，Shift+↑↓ 逐行，按其它键回到最新\n")
	}
	s.io.Printf("====================================================\n")
	s.updateStatus()
}

// prompt 返回当前模式的提示符（纯文本：自绘屏按模式自己上色，文本路径由 consoleIO 上色）。
func (s *Session) prompt() string {
	if s.cmdMode {
		return "$ "
	}
	return "你> "
}

// togglePromptMode 在 AI / 命令模式之间切换，并刷新状态栏与提示。
func (s *Session) togglePromptMode() {
	s.cmdMode = !s.cmdMode
	if s.cmdMode {
		s.io.promptANSI = ansiGreen
		s.io.Printf("%s\n", s.io.paint(ansiGreen, "（命令模式：输入直接执行本地命令；不经过模型，也不进模型上下文；Tab 切回）"))
	} else {
		s.io.promptANSI = ansiCyan
		s.io.Printf("%s\n", s.io.paint(ansiCyan, "（AI 模式：输入发给模型；Tab 切到命令模式）"))
	}
	s.Log.Note("prompt mode -> " + s.promptModeName())
	s.updateStatus()
}

func (s *Session) promptModeName() string {
	if s.cmdMode {
		return "cmd"
	}
	return "ai"
}

// setPhase 上报会话阶段（就绪 / 请求模型 / 生成中 / 调用工具 / 回答完毕）。
// 两种 UI 各用各的通道：自绘屏写底部状态栏（本地模型首字可能要十几秒，用户盯着静止画面
// 无法区分"在算"和"卡死"）；文本/串口控制台没有状态栏，只能打一行 dim 提示——它的 CSI
// 解析只支持 m/K/J，做不到原地刷一行。脚本模式是无人值守回归，输出要进日志比对，不打扰。
func (s *Session) setPhase(text string) {
	s.phase = text
	if s.io.scr != nil {
		s.updateStatus()
		return
	}
	if s.io.script {
		return
	}
	s.io.Printf("%s\n", s.io.paint(ansiDim, "  ● "+text))
}

// setPhaseOnScreen 只更新自绘屏状态栏：这类阶段在文本控制台上已有等价呈现
// （回答正逐字打出、工具调用本身有一行提示），再插一行只会打断阅读。
func (s *Session) setPhaseOnScreen(text string) {
	if s.io.scr != nil {
		s.setPhase(text)
	}
}

// markGenerating 在每轮流式响应的首个 token 上报一次"生成中"。
func (s *Session) markGenerating(st *streamState) {
	if st.phaseSeen {
		return
	}
	st.phaseSeen = true
	s.setPhaseOnScreen("生成中…")
}

// updateStatus 刷新自绘屏底部状态栏（文本路径没有状态栏，直接忽略）。
func (s *Session) updateStatus() {
	if s.io.scr == nil {
		return
	}
	s.io.scr.SetStatus(s.statusLine())
}

// statusLine 拼装状态栏文本：阶段（就绪/请求模型/生成中/回答完毕·耗时）│ 模型或命令模式目录 │ 模式与按键提示。
func (s *Session) statusLine() string {
	left := "[AI] " + s.Cfg.Model
	if s.cmdMode {
		left = "[命令] " + s.cwd
	}
	ime := ""
	if screen.IMEAvailable() {
		ime = " │ Ctrl+Space 中/英"
	}
	phase := ""
	if s.phase != "" {
		phase = s.phase + " │ "
	}
	return fmt.Sprintf(
		" %s%s · 安全模式 %s │ Tab 补全 │ Ctrl+T 切换模式 │ ↑↓ 历史%s ", phase, left, s.Ctx.Mode, ime)
}

func (s *Session) autoProbe() error {
	res, err := s.Reg.Call(s.Ctx, "system_probe", json.RawMessage("{}"))
	if err != nil {
		return err
	}
	s.Log.ToolResult("system_probe", res, "")
	s.history = append(s.history, llm.Message{Role: "user", Content: "【系统探测结果（自动执行 system_probe）】\n" + res})
	s.probeInHistory = res
	return nil
}

// dedupProbe 让同一份探测结果在模型上下文里只出现一次：开场 autoProbe 已整段注入，
// 模型随后再调 system_probe 时若环境未变（结果逐字相同），只回一句指针说明。
// 一份探测结果实测 3k~7k token，重复一次就足以撑爆本地模型的上下文窗口。
// 环境真的变了（挂载、分区写完之后的再探测）返回完整结果，不做任何摘要。
func (s *Session) dedupProbe(name, result string) string {
	if name != "system_probe" {
		return result
	}
	if s.probeInHistory != "" && result == s.probeInHistory {
		return "（与上文【系统探测结果（自动执行 system_probe）】完全相同，不再重复给出；系统已变更时重新调用会返回新结果）"
	}
	s.probeInHistory = result
	return result
}

// closeScreen 恢复文本控制台。正常结束时保留最后一屏画面（随后系统关机），
// 仅出错路径调用，以便错误信息能在文本控制台上看到。
func (s *Session) closeScreen() {
	if s.io != nil && s.io.scr != nil {
		s.io.scr.Close()
	}
}

func (s *Session) Run() (err error) {
	defer func() {
		if err != nil {
			s.closeScreen()
		}
	}()
	s.printBanner()
	if err := s.autoProbe(); err != nil {
		s.io.Printf("（自动探测失败: %v）\n", err)
		s.setPhase("就绪（自动探测失败）")
	} else {
		s.setPhase("就绪")
	}
	for {
		s.io.SetHistoryKind(s.promptModeName())
		line, err2 := s.io.readLine(s.prompt())
		if err2 != nil {
			if err2 == io.EOF {
				s.io.Printf("\n再见。\n")
				return nil
			}
			return err2
		}
		if s.io.ConsumeTabToggle() { // Tab：切换模式，已输入的文本带过去
			s.io.SetPrefill(line)
			s.togglePromptMode()
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if isExitWord(line) {
			s.io.Printf("再见。\n")
			return nil
		}
		if strings.HasPrefix(line, "/") {
			if quit := s.handleCommand(line); quit {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "!") {
			s.runShellLine(strings.TrimSpace(strings.TrimPrefix(line, "!")))
			continue
		}
		if s.cmdMode { // 命令模式：非 / 非 ! 的行直接当命令执行
			s.runShellLine(line)
			continue
		}
		if err := s.Turn(line); err != nil {
			s.io.Printf("（出错: %v）\n", err)
			s.Log.Note("turn error: " + err.Error())
		}
	}
}

func isExitWord(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "exit", "quit", "退出":
		return true
	}
	return false
}

func (s *Session) RunScript(lines []string) (err error) {
	defer func() {
		if err != nil {
			s.closeScreen()
		}
	}()
	s.printBanner()
	s.io.Printf("（脚本模式：共 %d 条输入）\n", len(lines))
	if err := s.autoProbe(); err != nil {
		s.io.Printf("（自动探测失败: %v）\n", err)
	}
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s.io.Printf("\n[脚本 %d/%d] 用户> %s\n", i+1, len(lines), line)
		if isExitWord(line) {
			s.io.Printf("（收到退出命令，脚本提前结束）\n")
			return nil
		}
		if strings.HasPrefix(line, "/") {
			if s.handleCommand(line) {
				s.io.Printf("（收到退出命令，脚本提前结束）\n")
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "!") {
			s.runShellLine(strings.TrimSpace(strings.TrimPrefix(line, "!")))
			continue
		}
		if err := s.Turn(line); err != nil {
			s.io.Printf("（出错: %v）\n", err)
			s.Log.Note("turn error: " + err.Error())
		}
	}
	s.io.Printf("\n（脚本执行完毕，日志: %s.md）\n", s.Log.Path)
	return nil
}

func (s *Session) Turn(input string) error {
	s.Log.Msg("user", input)
	s.io.AddHistory("ai", input)
	s.history = append(s.history, llm.Message{Role: "user", Content: input})
	s.turnStart = time.Now()

	for i := 0; i < maxToolItersPerTurn; i++ {
		s.trimHistory()
		if i == 0 {
			s.setPhase("请求模型…")
		} else {
			s.setPhase("继续推理…")
		}
		st := s.io.beginStream()
		resp, finishReason, err := s.Client.Chat(s.history, s.Reg.Defs(), &llm.StreamHandler{
			OnReasoning: func(d string) { s.markGenerating(st); s.io.streamReasoning(st, d) },
			OnContent:   func(d string) { s.markGenerating(st); s.io.streamContent(st, d) },
		})
		s.io.endStream(st)
		if err != nil {
			s.setPhase(fmt.Sprintf("出错 · %s", s.turnElapsed()))
			return err
		}
		if resp.Role == "" {
			resp.Role = "assistant"
		}
		if len(resp.ToolCalls) == 0 {
			text := strings.TrimSpace(resp.Content)
			if text == "" {
				switch {
				case finishReason == "length":
					s.io.Printf("（⚠ 输出被 max_tokens 截断：思考内容吃满预算、正文为空，请调大 ai.json 的 max_tokens）\n")
					s.Log.Note("模型输出被 max_tokens 截断（finish_reason=length）")
				case finishReason != "":
					s.io.Printf("（模型未返回内容，finish_reason=%s）\n", finishReason)
				default:
					s.io.Printf("（模型未返回内容）\n")
				}
			}
			s.Log.Msg("assistant", text)
			s.history = append(s.history, llm.Message{Role: "assistant", Content: text})
			s.setPhase(fmt.Sprintf("回答完毕 · %s", s.turnElapsed()))
			return nil
		}
		for j := range resp.ToolCalls {
			if resp.ToolCalls[j].Type == "" {
				resp.ToolCalls[j].Type = "function"
			}
		}
		if strings.TrimSpace(resp.Content) != "" {
			s.Log.Msg("assistant", resp.Content)
		}
		s.history = append(s.history, *resp)

		for _, tc := range resp.ToolCalls {
			name := tc.Function.Name
			argText := tc.Function.Arguments
			if strings.TrimSpace(argText) == "" {
				argText = "{}"
			}
			s.io.Printf("%s\n", s.io.toolCallLine(name, argText))
			s.setPhaseOnScreen(fmt.Sprintf("调用 %s…", name))
			s.Log.ToolCall(name, argText)
			start := time.Now()
			result, terr := s.Reg.Call(s.Ctx, name, json.RawMessage(argText))
			dur := time.Since(start)
			errStr := ""
			if terr != nil {
				errStr = terr.Error()
				result = "错误: " + errStr
			}
			s.Log.ToolResult(name, result, errStr)
			s.io.toolResult(result, errStr, dur)
			s.history = append(s.history, llm.Message{Role: "tool", ToolCallID: tc.ID, Name: name, Content: s.dedupProbe(name, result)})
		}
	}
	s.setPhase(fmt.Sprintf("出错 · %s", s.turnElapsed()))
	return fmt.Errorf("单轮工具调用达到上限 %d 次，已停止本轮", maxToolItersPerTurn)
}

// turnElapsed 返回本轮已用时（秒，保留一位小数），用于状态栏的"回答完毕/出错"耗时。
func (s *Session) turnElapsed() string {
	if s.turnStart.IsZero() {
		return "?"
	}
	return fmt.Sprintf("%.1fs", time.Since(s.turnStart).Seconds())
}

func (s *Session) trimHistory() {
	if len(s.history) <= maxHistoryMessages {
		return
	}
	for cut := 1; cut < len(s.history); cut++ {
		if s.history[cut].Role == "user" && len(s.history)-cut <= historyKeepTail {
			s.history = append([]llm.Message{s.history[0]}, s.history[cut:]...)
			s.forgetProbeIfTrimmed()
			return
		}
	}
}

// forgetProbeIfTrimmed 裁剪可能把探测原文整段裁掉；此时必须解除去重标记，
// 否则后续 system_probe 只回"见上文"，而上文已无该内容，模型彻底看不到系统信息。
// 原文有两种进历史的方式：开场 user 消息（带前缀）与 system_probe 的 tool 消息（即原文）。
func (s *Session) forgetProbeIfTrimmed() {
	if s.probeInHistory == "" {
		return
	}
	for _, m := range s.history {
		if strings.Contains(m.Content, s.probeInHistory) {
			return
		}
	}
	s.probeInHistory = ""
}

// runShellLine 执行用户在提示符直接用 `!` 前缀输入的命令，复用 run_command 工具的
// 安全层（只读白名单直执 / orchestrate 需确认 / readonly 拒绝写类命令 / 输出截断 /
// 超时上限），不经过模型。命令与输出只进屏幕与日志，不进模型历史。
func (s *Session) runShellLine(line string) {
	if line == "" {
		s.io.Printf("用法: !命令 [参数...]（不经 shell，不支持引号/管道/重定向）\n")
		return
	}
	fields := strings.Fields(line)
	s.io.AddHistory("cmd", line)
	if fields[0] == "cd" { // cd 是会话内建（改 run_command 的工作目录），不下发执行
		s.changeDir(fields[1:])
		return
	}
	args := map[string]any{"cmd": fields[0]}
	if len(fields) > 1 {
		args["args"] = fields[1:]
	}
	raw, err := json.Marshal(args)
	if err != nil {
		s.io.Printf("构造参数失败: %v\n", err)
		return
	}
	s.Log.Note("用户直执: " + line)
	res, err := s.Reg.Call(s.Ctx, "run_command", raw)
	if err != nil {
		s.io.Printf("执行失败: %v\n", err)
		return
	}
	s.io.Printf("%s\n", res)
}

// changeDir 处理命令模式的 cd：改会话工作目录（run_command 与路径补全都跟着变）。
func (s *Session) changeDir(args []string) {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	dir, err := resolveCd(s.cwd, target)
	if err != nil {
		s.io.Printf("%v\n", err)
		return
	}
	s.cwd = dir
	s.Ctx.Cwd = dir
	s.io.SetCwd(dir)
	s.Log.Note("cwd -> " + dir)
	s.io.Printf("现在位于 %s\n", dir)
	s.updateStatus()
}

// resolveCd 把 cd 参数解析为绝对路径（相对路径基于 cwd；无参数回根目录）。
// 目标必须存在且是目录。
func resolveCd(cwd, arg string) (string, error) {
	if cwd == "" {
		cwd = "/"
	}
	if arg == "" {
		return "/", nil
	}
	p := arg
	if !strings.HasPrefix(p, "/") {
		p = path.Join(cwd, p)
	}
	p = path.Clean(p)
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("无法进入 %s: %v", arg, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s 不是目录", arg)
	}
	return p, nil
}

func (s *Session) handleCommand(line string) bool {
	parts := strings.Fields(line)
	switch parts[0] {
	case "/quit", "/exit", "/q":
		s.io.Printf("再见。\n")
		return true
	case "/help":
		s.io.Printf(`命令:
  /help              显示帮助
  /probe             重新探测系统与 U 盘内容
  /mode <模式>       切换安全模式（readonly/orchestrate/direct）
  /undo              还原 ventoy.json（从 ventoy.json.bak）
  /log               显示日志文件路径
  !命令 [参数...]    直接执行本地命令（不经模型；shell 类命令如 sh/python 需 direct 模式）
  cd <目录>          切换工作目录（相对路径基于当前目录；run_command 与补全都用它）
  Ctrl+T             在 AI / 命令模式间切换（命令模式提示符为 $）
  Tab                命令补全（命令名 / 路径；多个候选时再按一次 Tab 列出）
  Ctrl-Space         切换中/英输入法
  /cmd /ai           同 Ctrl+T（串口/文本控制台路径用这两个；那边 Shift+Tab 也可用）
  PageUp/PageDown    回看历史（Shift+↑↓ 逐行，按其它键回到最新；仅自绘屏幕）
  exit / quit / 退出  结束对话（等同于 /quit）
  /quit              退出
`)
	case "/cmd", "/sh":
		if s.cmdMode {
			s.io.Printf("已在命令模式（输入直接执行命令，/ai 或 Tab 切回）\n")
		} else {
			s.togglePromptMode()
		}
	case "/ai":
		if s.cmdMode {
			s.togglePromptMode()
		} else {
			s.io.Printf("已在 AI 模式（输入发给模型，/cmd 或 Tab 切到命令模式）\n")
		}
	case "/probe":
		res, err := s.Reg.Call(s.Ctx, "system_probe", json.RawMessage("{}"))
		if err != nil {
			s.io.Printf("探测失败: %v\n", err)
		} else {
			s.io.Printf("%s\n", res)
		}
		s.Log.Note("手动执行 system_probe")
	case "/mode":
		if len(parts) < 2 {
			s.io.Printf("用法: /mode readonly|orchestrate|direct\n")
			break
		}
		switch parts[1] {
		case "readonly", "orchestrate", "direct":
			s.Ctx.Mode = parts[1]
			s.io.Printf("已切换为 %s 模式\n", parts[1])
			s.Log.Note("mode -> " + parts[1])
			s.updateStatus()
		default:
			s.io.Printf("未知模式: %s\n", parts[1])
		}
	case "/undo":
		s.undo()
	case "/log":
		s.io.Printf("日志: %s.jsonl / %s.md\n", s.Log.Path, s.Log.Path)
	default:
		s.io.Printf("未知命令: %s（/help 查看帮助）\n", parts[0])
	}
	return false
}

func (s *Session) undo() {
	vjson := filepath.Join(s.Ctx.PayloadRoot(), "ventoy", "ventoy.json")
	bak := vjson + ".bak"
	data, err := os.ReadFile(bak)
	if err != nil {
		s.io.Printf("没有可用的备份 %s\n", bak)
		return
	}
	if err := os.WriteFile(vjson, data, 0644); err != nil {
		s.io.Printf("还原失败: %v\n", err)
		return
	}
	s.io.Printf("已从 %s 还原 ventoy.json\n", bak)
	s.Log.Note("undo: restored " + vjson)
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
