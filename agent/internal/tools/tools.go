package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"os-pilot-ai/internal/disk"
	"os-pilot-ai/internal/llm"
)

var ErrNoInput = errors.New("当前模式不支持交互输入")

type IO interface {
	Printf(format string, args ...any)
	Ask(prompt string) (string, error)
	Confirm(prompt, detail string) (bool, error)
	TypedConfirm(prompt, detail, word string) (bool, error)
	ScriptMode() bool
}

type Context struct {
	PayloadDir string
	Mode       string
	NoReboot   bool
	Cwd        string // run_command 的工作目录（空 = /；由 session 命令模式的 cd 维护）
	IO         IO
	Reboot     func() error
	Disks      *disk.Probe // 磁盘业务层；nil 时各 handler 回退到 disk.DefaultProbe()
}

func (c *Context) WriteAllowed() bool {
	return c.Mode == "orchestrate" || c.Mode == "direct"
}

// WorkDir 返回 run_command 的工作目录。
func (c *Context) WorkDir() string {
	if c.Cwd == "" {
		return "/"
	}
	return c.Cwd
}

func (c *Context) PayloadRoot() string {
	if c.PayloadDir == "" {
		return "/iso"
	}
	return c.PayloadDir
}

type Handler func(ctx *Context, raw json.RawMessage) (string, error)

type tool struct {
	def     llm.ToolDef
	handler Handler
}

type Registry struct {
	tools map[string]tool
	order []string
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]tool)}
}

func (r *Registry) register(def llm.ToolDef, h Handler) {
	r.tools[def.Function.Name] = tool{def: def, handler: h}
	r.order = append(r.order, def.Function.Name)
}

func (r *Registry) Defs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.tools[name].def)
	}
	return defs
}

func (r *Registry) Call(ctx *Context, name string, raw json.RawMessage) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("未知工具: %s", name)
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	return t.handler(ctx, raw)
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func NewDefaultRegistry() *Registry {
	r := NewRegistry()

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "system_probe",
		Description: "探测当前运行环境：内核/CPU/内存、网络、块设备(blkid/partitions)、挂载点，以及数据分区(payload_dir)内的文件清单和 ventoy.json 内容。回答用户前应先调用它了解现状。",
		Parameters:  objSchema(map[string]any{}),
	}}, handleSystemProbe)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "fs_read",
		Description: "读取数据分区内的文件或列出目录。路径可写绝对路径或相对 payload_dir 的相对路径，只允许访问 payload_dir 之内。",
		Parameters: objSchema(map[string]any{
			"path":      map[string]any{"type": "string", "description": "文件或目录路径"},
			"max_bytes": map[string]any{"type": "integer", "description": "最多读取字节数，默认 262144，上限 1048576"},
		}, "path"),
	}}, handleFSRead)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "fs_write",
		Description: "写入数据分区内的普通文件（如应答脚本）。会自动备份原文件为 *.bak，展示 diff 并请求确认（direct 模式除外）。修改 ventoy.json 请改用 schedule_boot 工具。",
		Parameters: objSchema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "目标文件路径（payload_dir 之内）"},
			"content": map[string]any{"type": "string", "description": "要写入的内容"},
			"append":  map[string]any{"type": "boolean", "description": "true 表示追加到文件末尾，默认 false（覆盖）"},
		}, "path", "content"),
	}}, handleFSWrite)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "run_command",
		Description: "执行一个只读或经用户确认的本地命令（不经过 shell，无管道/重定向）。只读命令（blkid/fdisk/lspci/lsusb/dmesg/ls/cat/ip 等）可直接执行；其他命令在 orchestrate 模式下需要用户确认。禁止 shell 类命令。",
		Parameters: objSchema(map[string]any{
			"cmd":         map[string]any{"type": "string", "description": "命令名，不含路径，如 blkid、ls"},
			"args":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "参数列表（逐个传参，不做 shell 展开）"},
			"timeout_sec": map[string]any{"type": "integer", "description": "超时秒数，默认 30，上限 120"},
		}, "cmd"),
	}}, handleRunCommand)

	// 能力文案只从 disk 的 fsSpecs 表 + 随包工具探测生成：这里不出现第二份硬编码列表，
	// 随包工具缺席时（例如没编 mkntfs）模型看到的清单会自动少一项，不会“先承诺再失败”。
	formatFSTypes := disk.FormatFSAdvertised()
	partitionFSTypes := disk.SupportedFS()
	noCreateFS := strings.Join(disk.NonCreatableFS(), "/")

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "ask_user",
		Description: "向用户提问并等待回答。用于询问安装目标系统、分区方案、用户名、时区等关键信息。可提供选项列表。",
		Parameters: objSchema(map[string]any{
			"question": map[string]any{"type": "string", "description": "问题文本"},
			"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选项（可为空）"},
		}, "question"),
	}}, handleAskUser)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "schedule_boot",
		Description: "写入 Ventoy 配置(ventoy.json)：把指定镜像设为默认启动项、设置菜单超时，并可挂接自动安装模板（autosel/timeout=-1 表示下次开机不显示菜单直接开始无人值守安装）。写入前会备份为 ventoy.json.bak 并请求用户输入 REBOOT 确认；随后触发重启。",
		Parameters: objSchema(map[string]any{
			"image":    map[string]any{"type": "string", "description": "要安装的镜像路径（payload_dir 内，如 /ubuntu-24.04.iso）"},
			"template": map[string]any{"type": "string", "description": "自动安装应答脚本路径（payload_dir 内）。留空则只设置默认启动项，不启用无人值守"},
			"autosel":  map[string]any{"type": "integer", "description": "应答模板序号（从 1 开始），默认 1"},
			"timeout":  map[string]any{"type": "integer", "description": "模板选择菜单超时秒数。负数或 0（默认 -1）表示不显示模板菜单、直接开始无人值守安装（写入时省略该字段：Ventoy 的 JSON 解析器不支持负数）"},
		}, "image"),
	}}, handleScheduleBoot)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "list_disks",
		Description: "列出所有块设备（整盘与分区）及其结构化信息：容量、型号、总线、分区表、每个分区的文件系统/卷标/UUID/挂载点。protected=true 表示该设备承载运行中的 AI 环境（数据分区/根）或已被挂载，禁止改动。改分区/格式化前必须先调用它选定目标盘。",
		Parameters:  objSchema(map[string]any{}),
	}}, handleListDisks)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "partition",
		Description: "对整盘重建分区表并创建分区（会清空该盘原有分区表与数据）。disk 传整盘设备（如 /dev/sdb，不能是 /dev/sdb1）；partitions 按顺序描述每块：size 形如 100MiB/8G/rest（rest 只能放最后）。执行前会展示将运行的 parted 命令并要求用户逐字输入盘名确认。",
		Parameters: objSchema(map[string]any{
			"disk":  map[string]any{"type": "string", "description": "要分区的整盘设备路径，如 /dev/sdb"},
			"table": map[string]any{"type": "string", "description": "分区表类型 gpt 或 msdos，默认 gpt"},
			"partitions": map[string]any{
				"type":        "array",
				"description": "分区列表，按先后顺序分配（首块从 1MiB 起，保证对齐）",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"size": map[string]any{"type": "string", "description": "大小，如 100MiB / 8G / rest / 100%"},
						"fs":   map[string]any{"type": "string", "enum": partitionFSTypes, "description": "分区类型提示：只决定 parted 的 fs-type 记号与 GPT 分区类型，不影响后续格式化。EFI 系统分区用 vfat；跨平台数据盘用 exfat（parted 无该记号，会自动标成 Microsoft Basic Data）"},
						"name": map[string]any{"type": "string", "description": "GPT 分区名（可选）"},
					},
					"required": []string{"size"},
				},
			},
		}, "disk", "partitions"),
	}}, handlePartition)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "format",
		Description: fmt.Sprintf("在分区（或裸盘）上创建文件系统，会清空该设备原有数据。可格式化的类型见 fstype 的 enum（由随包工具探测得出，缺席即不支持新建）；%s 只能读写挂载与分区，Linux 端没有创建工具，遇到这类诉求要改口建议 exfat。用途选择：Linux 装机用 ext4，EFI 系统分区用 vfat，Windows/macOS/Android 之间互访的数据盘用 exfat。device 传具体分区（如 /dev/sdb1）；已挂载的分区需先卸载。执行前展示将运行的 mkfs 命令并要求用户逐字输入设备名确认。", noCreateFS),
		Parameters: objSchema(map[string]any{
			"device": map[string]any{"type": "string", "description": "要格式化的设备路径，如 /dev/sdb1"},
			"fstype": map[string]any{"type": "string", "enum": formatFSTypes, "description": "文件系统类型，默认 ext4；只能取 enum 里的值"},
			"label":  map[string]any{"type": "string", "description": "卷标（可选；长度上限按类型：vfat 11 / exfat 15 / 其余 16 字符）"},
		}, "device"),
	}}, handleFormat)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "backup",
		Description: "用 rsync 把 src 目录（默认数据分区 payload_dir）的内容备份到 dst 目录。dst 所在文件系统不得是运行中的系统/数据盘；delete=true 会删除目标端多余文件，此时要求 dst 本身是挂载点。执行前展示将运行的 rsync 命令并请求确认。",
		Parameters: objSchema(map[string]any{
			"src":     map[string]any{"type": "string", "description": "源目录，默认 payload_dir"},
			"dst":     map[string]any{"type": "string", "description": "目标目录（须已存在；通常是刚格式化并挂载的备份盘挂载点）"},
			"delete":  map[string]any{"type": "boolean", "description": "是否删除目标端多余文件（--delete），默认 false"},
			"dry_run": map[string]any{"type": "boolean", "description": "只试算不实际传输（--dry-run），默认 false"},
		}, "dst"),
	}}, handleBackup)

	r.register(llm.ToolDef{Type: "function", Function: llm.FunctionDef{
		Name:        "gen_autoinstall",
		Description: "确定性生成 Ubuntu autoinstall 应答脚本（cloud-config）。选盘用目标盘的磁盘序列号（serial）、挂载用卷标（label），因此不依赖 /dev/vdX、/dev/sdX 这类会变的设备名。整个模板由代码拼装、字段符合 curtin schema；不要手写同类模板（手写常出现硬编码设备名与非法字段）。生成后用 schedule_boot 把它挂到镜像上即可。",
		Parameters: objSchema(map[string]any{
			"image": map[string]any{"type": "string", "description": "数据分区内的镜像路径（如 /ISO/ubuntu-24.04.iso）"},
			"disk":  map[string]any{"type": "string", "description": "目标整盘设备（如 /dev/sdb）；会取它的序列号写进模板。不能是承载运行环境的盘"},
			"partitions": map[string]any{
				"type":        "array",
				"description": "分区列表（按顺序）；必须有且只有一个挂载到 /；rest 只能放最后一项",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"size":  map[string]any{"type": "string", "description": "大小：512M / 8G / rest"},
						"label": map[string]any{"type": "string", "description": "卷标（必填；用于 fstab 稳定挂载），≤16 字符"},
						"mount": map[string]any{"type": "string", "description": "挂载点：/ 或 /home 或 /boot/efi"},
						"fs":    map[string]any{"type": "string", "description": "文件系统 ext4/vfat（留空则按挂载点推断：/boot/efi→vfat，其余 ext4）"},
						"boot":  map[string]any{"type": "boolean", "description": "是否加 boot 标志（EFI 分区设 true）"},
					},
					"required": []string{"size", "label", "mount"},
				},
			},
			"output":        map[string]any{"type": "string", "description": "模板输出路径（payload_dir 内；默认 autoinstall-<镜像名>.yaml）"},
			"username":      map[string]any{"type": "string", "description": "用户名，默认 ubuntu"},
			"hostname":      map[string]any{"type": "string", "description": "主机名，默认 ubuntu"},
			"password_hash": map[string]any{"type": "string", "description": "密码的 crypt 哈希（如 $6$...）；留空则账号锁定，安装后需自行设密码"},
			"locale":        map[string]any{"type": "string", "description": "区域，默认 zh_CN.UTF-8"},
			"timezone":      map[string]any{"type": "string", "description": "时区，默认 Asia/Shanghai"},
		}, "image", "disk", "partitions"),
	}}, handleGenAutoinstall)

	return r
}

func resolvePayloadPath(ctx *Context, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	root, err := filepath.Abs(ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(root, p))
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("路径解析失败: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 超出允许范围（仅限 %s 内）", p, root)
	}
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		if r2, err := filepath.Rel(root, resolved); err != nil || r2 == ".." || strings.HasPrefix(r2, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("路径 %q 经符号链接后超出允许范围", p)
		}
	}
	return full, nil
}

type fsReadArgs struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
}

// normalizePayloadPath 把 image/template 归一化为 ventoy.json 的存储形式 "/x"。
// 接受三种写法：guest 绝对路径（/iso/x）、带前导斜杠的 payload 相对路径（/x）、裸相对路径（x）。
func normalizePayloadPath(ctx *Context, p string) (string, error) {
	s := strings.TrimSpace(p)
	if s == "" {
		return "", fmt.Errorf("path 不能为空")
	}
	s = filepath.ToSlash(s)
	root := filepath.ToSlash(filepath.Clean(ctx.PayloadRoot()))
	if root != "/" && root != "." {
		if s == root {
			s = "/"
		} else if strings.HasPrefix(s, root+"/") {
			s = strings.TrimPrefix(s, root)
		}
	}
	s = filepath.ToSlash(filepath.Clean("/" + strings.TrimPrefix(s, "/")))
	if s == "/" {
		return "", fmt.Errorf("path 不能为 payload 根目录")
	}
	return s, nil
}

func handleFSRead(ctx *Context, raw json.RawMessage) (string, error) {
	var a fsReadArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	full, err := resolvePayloadPath(ctx, a.Path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("无法访问 %s: %w", a.Path, err)
	}

	if st.IsDir() {
		ents, err := os.ReadDir(full)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "目录 %s（%d 项）:\n", a.Path, len(ents))
		for i, e := range ents {
			if i >= 300 {
				fmt.Fprintf(&b, "...（还有 %d 项）\n", len(ents)-i)
				break
			}
			kind := "文件"
			if e.IsDir() {
				kind = "目录"
			}
			extra := ""
			if info, err := e.Info(); err == nil {
				extra = fmt.Sprintf("  %d 字节  %s", info.Size(), info.ModTime().Format("2006-01-02 15:04"))
			}
			fmt.Fprintf(&b, "  [%s] %s%s\n", kind, e.Name(), extra)
		}
		return b.String(), nil
	}

	max := a.MaxBytes
	if max <= 0 {
		max = 256 * 1024
	}
	if max > 1<<20 {
		max = 1 << 20
	}
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := readAtMost(f, max)
	if err != nil {
		return "", err
	}

	if idx := bytes.IndexByte(data, 0); idx >= 0 && idx < 512 {
		head := data
		if len(head) > 256 {
			head = head[:256]
		}
		var b strings.Builder
		fmt.Fprintf(&b, "二进制文件 %s（%d 字节），前 %d 字节 hexdump:\n", a.Path, st.Size(), len(head))
		for i := 0; i < len(head); i += 16 {
			end := i + 16
			if end > len(head) {
				end = len(head)
			}
			fmt.Fprintf(&b, "%08x  %-48x  |%s|\n", i, head[i:end], printable(head[i:end]))
		}
		return b.String(), nil
	}

	text := string(data)
	if st.Size() > int64(len(data)) {
		text += fmt.Sprintf("\n...(文件共 %d 字节，已截断到 %d 字节)", st.Size(), len(data))
	}
	return text, nil
}

func readAtMost(f *os.File, max int) ([]byte, error) {
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 64*1024)
	for len(buf) < max {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	if len(buf) > max {
		buf = buf[:max]
	}
	return buf, nil
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 32 && c < 127 {
			out[i] = c
		} else {
			out[i] = '.'
		}
	}
	return string(out)
}

type fsWriteArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func handleFSWrite(ctx *Context, raw json.RawMessage) (string, error) {
	var a fsWriteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝写入请求）", nil
	}
	full, err := resolvePayloadPath(ctx, a.Path)
	if err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(ctx.PayloadRoot(), full)

	oldText := ""
	exists := false
	if b, err := os.ReadFile(full); err == nil {
		oldText = string(b)
		exists = true
	}
	newText := a.Content
	if a.Append && exists {
		newText = oldText + a.Content
	}
	if exists && oldText == newText {
		return "文件内容无变化，未写入。", nil
	}

	if ctx.Mode != "direct" {
		detail := UnifiedDiff(rel, oldText, newText)
		ok, err := ctx.IO.Confirm(fmt.Sprintf("写入 %s（%d → %d 字节）", rel, len(oldText), len(newText)), detail)
		if err != nil {
			return "", err
		}
		if !ok {
			return "用户拒绝了这次写入。", nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	if exists {
		if err := os.WriteFile(full+".bak", []byte(oldText), 0644); err != nil {
			return "", fmt.Errorf("备份失败: %w", err)
		}
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, []byte(newText), 0644); err != nil {
		return "", fmt.Errorf("写入失败: %w", err)
	}
	if err := os.Rename(tmp, full); err != nil {
		return "", fmt.Errorf("替换文件失败: %w", err)
	}

	msg := fmt.Sprintf("已写入 %s（%d 字节）", rel, len(newText))
	if exists {
		msg += "，原文件备份为 " + rel + ".bak"
	}
	return msg, nil
}

type runArgs struct {
	Cmd        string   `json:"cmd"`
	Args       []string `json:"args"`
	TimeoutSec int      `json:"timeout_sec"`
}

// shellLike 是"能间接执行任意命令"的 shell 类命令，默认禁止；仅 direct 模式放行
// （见 shellAllowed）。find 单独处理：普通 find 按只读放行，只有带上 -exec/-delete
// 等会执行命令或写文件的参数才拒绝（见 hasDangerousFindArg）。
var shellLike = map[string]bool{
	"sh": true, "ash": true, "bash": true, "dash": true, "ksh": true, "csh": true,
	"env": true, "xargs": true, "awk": true, "nohup": true,
	"setsid": true, "chroot": true, "nice": true, "ionice": true, "timeout": true,
	"flock": true, "su": true, "runuser": true, "setpriv": true, "doas": true,
	"watch": true, "busybox": true, "perl": true, "python": true, "python3": true,
}

// shellAllowed 判断当前安全模式下是否允许 shell 类命令：默认禁止，
// 只有 direct 模式（用户显式选择"直接执行、不确认"）才放行。
func shellAllowed(mode, cmd string) bool {
	return !shellLike[cmd] || mode == "direct"
}

// findDangerousArgs 是会让 find 变成"执行命令或写文件"的参数。
var findDangerousArgs = map[string]bool{
	"-exec": true, "-execdir": true, "-ok": true, "-okdir": true, "-delete": true,
	"-fls": true, "-fprint": true, "-fprint0": true, "-fprintf": true,
}

func hasDangerousFindArg(args []string) bool {
	for _, a := range args {
		if findDangerousArgs[a] {
			return true
		}
	}
	return false
}

const metaChars = ";|&`$><\n\r\\\"'*?(){}[]~!"

func checkCmdToken(s string) error {
	if s == "" {
		return fmt.Errorf("cmd 不能为空")
	}
	if strings.Contains(s, "/") {
		return fmt.Errorf("cmd %q 不允许包含路径分隔符", s)
	}
	for _, c := range s {
		if strings.ContainsRune(metaChars, c) {
			return fmt.Errorf("cmd %q 包含禁止字符 %q", s, c)
		}
	}
	return nil
}

var readCmds = map[string]bool{
	"blkid": true, "lsblk": true, "lspci": true, "lsscsi": true, "lsusb": true,
	"dmesg": true, "df": true, "free": true, "uname": true, "uptime": true,
	"cat": true, "ls": true, "stat": true, "readlink": true, "du": true,
	"date": true, "id": true, "hostname": true, "wc": true, "head": true,
	"tail": true, "grep": true, "hexdump": true, "od": true, "file": true,
	"sync": true, "sleep": true, "mdev": true, "mount": true, "ip": true,
	"ifconfig": true, "route": true, "find": true,
	// 工具链（小改内置）：dumpe2fs 天生只读；parted/tune2fs 也能写，逐个参数判定
	"dumpe2fs": true, "parted": true, "tune2fs": true,
}

func isReadOnlyCommand(cmd string, args []string) bool {
	if !readCmds[cmd] {
		return false
	}
	switch cmd {
	case "find":
		return !hasDangerousFindArg(args)
	case "mount":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
		return true
	case "ip":
		if len(args) == 0 {
			return true
		}
		for _, a := range args {
			switch a {
			case "set", "add", "del", "delete", "flush", "change", "replace", "up", "down":
				return false
			}
		}
		switch args[0] {
		case "addr", "a", "link", "l", "route", "r", "neigh", "n", "-s", "-br", "-j":
			return true
		}
		return false
	case "ifconfig":
		for _, a := range args {
			switch a {
			case "up", "down", "promisc", "-promisc":
				return false
			}
		}
		return true
	case "tune2fs":
		// 只有 -l/--list-superblock（只读列出超级块）算只读
		for _, a := range args {
			if a == "-l" || a == "--list-superblock" {
				return true
			}
		}
		return false
	case "parted":
		// 只有纯查询子命令算只读：print/unit/align-check/help；出现 mklabel/mkpart/rm 等即为写
		ro := map[string]bool{"print": true, "unit": true, "align-check": true, "help": true}
		seen := false
		for _, a := range args {
			if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "/") {
				continue
			}
			if !ro[a] {
				return false
			}
			seen = true
		}
		return seen
	}
	return true
}

func handleRunCommand(ctx *Context, raw json.RawMessage) (string, error) {
	var a runArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	cmd := strings.TrimSpace(a.Cmd)
	if err := checkCmdToken(cmd); err != nil {
		return "", err
	}
	if !shellAllowed(ctx.Mode, cmd) {
		return "（出于安全考虑，不允许执行 shell 类命令；仅 direct 模式放行）", nil
	}

	line := cmd
	if len(a.Args) > 0 {
		line += " " + strings.Join(a.Args, " ")
	}
	ro := isReadOnlyCommand(cmd, a.Args)

	if ctx.Mode == "readonly" && !ro {
		return "（readonly 模式：该命令不是只读命令，已拒绝）", nil
	}
	if ctx.Mode == "orchestrate" && !ro {
		ok, err := ctx.IO.Confirm("执行命令: "+line, "")
		if err != nil {
			return "", err
		}
		if !ok {
			return "用户拒绝了这次命令执行。", nil
		}
	}

	to := a.TimeoutSec
	if to <= 0 {
		to = 30
	}
	if to > 120 {
		to = 120
	}
	cctx, cancel := context.WithTimeout(context.Background(), time.Duration(to)*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, cmd, a.Args...)
	c.Env = []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin", "HOME=/root", "TERM=linux"}
	c.Dir = ctx.WorkDir()
	out, err := c.CombinedOutput()

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			return "", fmt.Errorf("执行 %s 失败: %w", cmd, err)
		}
	}
	s := string(out)
	if len(s) > 65536 {
		s = s[:65536] + "\n...(输出截断)"
	}
	return fmt.Sprintf("exit_code=%d\n%s", exitCode, s), nil
}

type askArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func handleAskUser(ctx *Context, raw json.RawMessage) (string, error) {
	var a askArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	prompt := a.Question
	if len(a.Options) > 0 {
		prompt += "\n选项: " + strings.Join(a.Options, " / ")
	}
	ans, err := ctx.IO.Ask(prompt)
	if err != nil {
		if errors.Is(err, ErrNoInput) {
			return "（当前无法交互提问，请基于已有信息继续）", nil
		}
		return "", err
	}
	if strings.TrimSpace(ans) == "" {
		return "（用户没有回答）", nil
	}
	return "用户回答: " + ans, nil
}
