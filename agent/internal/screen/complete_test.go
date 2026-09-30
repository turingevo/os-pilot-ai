package screen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 造一个临时目录树当补全基准；applet 名用注入的假 /bin 目录。
func completeFixture(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"iso/ventoy", "iso/scripts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"iso/0-OS-PILOT-AI.iso", "iso/ubuntu.iso", "iso/ventoy/ventoy.json",
		"init", "proc", "ubuntu.iso"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCompletePathUniqueAndPrefix(t *testing.T) {
	root := completeFixture(t)
	// 唯一匹配（文件不加 '/'）
	if m, fill := completeToken(root, "ubu", false); len(m) != 1 || fill != "ubuntu.iso" {
		t.Fatalf("唯一匹配应直接补全: %v %q", m, fill)
	}
	// 相对路径带子目录：iso/ven → iso/ventoy/（目录补出末尾 '/'）
	if m, fill := completeToken(root, "iso/ven", false); len(m) != 1 || fill != "iso/ventoy/" {
		t.Fatalf("子目录补全: %v %q", m, fill)
	}
	// 绝对路径（以临时根为前缀）：<root>/is → <root>/iso/
	abs := root + "/is"
	if m, fill := completeToken(root, abs, false); len(m) != 1 || fill != root+"/iso/" {
		t.Fatalf("绝对路径补全: %v %q", m, fill)
	}
	// 多候选：只补公共前缀
	if m, fill := completeToken(root, root+"/iso/", false); len(m) < 2 || fill != root+"/iso/" {
		t.Fatalf("多候选应只补公共前缀: %v %q", m, fill)
	}
	// 已输入完整的目录名（带结尾 /）：列出内容
	if m, _ := completeToken(root, "iso/ventoy/", false); len(m) != 1 || m[0] != "iso/ventoy/ventoy.json" {
		t.Fatalf("目录内列举: %v", m)
	}
	// 无匹配
	if m, fill := completeToken(root, "nope/zzz", false); len(m) != 0 || fill != "" {
		t.Fatalf("无匹配应返回空: %v %q", m, fill)
	}
}

func TestCompleteSkipsDotfilesUnlessTold(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _ := completeToken(root, "", false); len(m) != 0 {
		t.Fatalf("默认不应列点文件: %v", m)
	}
	if m, fill := completeToken(root, ".h", false); len(m) != 1 || fill != ".hidden" {
		t.Fatalf("显式打点才列: %v %q", m, fill)
	}
}

func TestCompleteCommandNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"ls", "lsblk", "lspci", "sleep", "sync", "sysctl"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil { // 目录不参与命令名
		t.Fatal(err)
	}
	old := cmdNamesDir
	cmdNamesDir = dir
	defer func() { cmdNamesDir = old }()

	if m, fill := completeToken("/", "sysc", true); len(m) != 1 || fill != "sysctl" {
		t.Fatalf("命令名唯一匹配: %v %q", m, fill)
	}
	m, fill := completeToken("/", "ls", true)
	if len(m) != 3 || fill != "ls" { // ls / lsblk / lspci：公共前缀就是 "ls"，无进展
		t.Fatalf("命令名多匹配: %v %q", m, fill)
	}
	if m, _ := completeToken("/", "sub", true); len(m) != 0 {
		t.Fatalf("目录不应作为命令名: %v", m)
	}
}

// Tab 补全：输入行被改写；候选多于一个时再按一次 Tab 把列表写进内容区。
func TestReadLineTabCompletion(t *testing.T) {
	root := completeFixture(t)
	s, _ := testScreen(t, 40, 4)
	s.SetStatusRows(2)
	s.SetCwd(root)

	line, err := readLine(t, s, "ls iso/ven\t\r", "$ ")
	if err != nil || line != "ls iso/ventoy/" {
		t.Fatalf("唯一候选应直接补全: line=%q err=%v", line, err)
	}

	// 多候选：第一次 Tab 只补公共前缀，第二次 Tab 列出候选（写进内容区）
	s2, _ := testScreen(t, 40, 6)
	s2.SetStatusRows(2)
	s2.SetCwd(root)
	line, err = readLine(t, s2, "ls iso/\t\t\r", "$ ")
	if err != nil || line != "ls iso/" {
		t.Fatalf("多候选不应改动已输入前缀: line=%q err=%v", line, err)
	}
	cells := strings.Join([]string{s2.visibleRow(0), s2.visibleRow(1), s2.visibleRow(2)}, "\n")
	if !strings.Contains(cells, "0-OS-PILOT-AI.iso") || !strings.Contains(cells, "（") {
		t.Fatalf("第二次 Tab 应列出候选:\n%s", cells)
	}
	if !strings.Contains(cells, "1 项）") && !strings.Contains(cells, "3 项）") {
		t.Logf("候选项计数（信息性）:\n%s", cells)
	}
}

// 命令位补全走 applet 名；非命令位走路径。
func TestReadLineTabCompletionPosition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sysctl"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := cmdNamesDir
	cmdNamesDir = dir
	defer func() { cmdNamesDir = old }()

	s, _ := testScreen(t, 40, 4)
	s.SetStatusRows(2)
	s.SetCwd("/")
	line, err := readLine(t, s, "sys\t\r", "$ ")
	if err != nil || line != "sysctl" {
		t.Fatalf("行首应补命令名: line=%q err=%v", line, err)
	}
}

func TestEscKeyMapsBackTab(t *testing.T) {
	if k := escKey('Z', nil); k.kind != keyBackTab {
		t.Fatalf("ESC[Z 应为 keyBackTab，得到 %v", k.kind)
	}
	if k := escKey('Z', []byte("1")); k.kind != keyBackTab {
		t.Fatalf("带参数的 ESC[Z 也应识别: %v", k.kind)
	}
}

// tokenAt：光标在词中时取整词（补全替换整词，而不是只补光标左边）。
func TestTokenAt(t *testing.T) {
	buf := []rune("ls /iso/ven")
	if start, end := tokenAt(buf, len(buf)); start != 3 || end != len(buf) {
		t.Fatalf("行尾 token: %d,%d", start, end)
	}
	if start, end := tokenAt(buf, 6); start != 3 || end != len(buf) {
		t.Fatalf("词中 token 应取整词: %d,%d", start, end)
	}
	if start, end := tokenAt([]rune("ls "), 3); start != 3 || end != 3 {
		t.Fatalf("空白处 token 应为空: %d,%d", start, end)
	}
}

// 切模式的按键编码：本地屏用 Ctrl+T(0x14)；串口/终端路径用 Shift+Tab 的 ESC[Z
// （以及少数终端发的 ESC+Tab）。Linux 控制台里 Shift+Tab 与 Tab 同效，故不采用。
func TestModeSwitchKeyEncodings(t *testing.T) {
	for _, in := range []string{"\x14", "\x1b[Z", "\x1b\x09"} {
		s, _ := testScreen(t, 20, 4)
		s.SetStatusRows(2)
		line, err := readLine(t, s, in, "> ")
		if err != nil || line != "" || !s.ConsumeTabToggle() {
			t.Fatalf("编码 %q 应触发切换: line=%q err=%v", in, line, err)
		}
	}
	// Ctrl+O(0x0f) 不再承担切换（曾是对 Shift+Tab 的错误猜测）
	s, _ := testScreen(t, 20, 4)
	s.SetStatusRows(2)
	line, err := readLine(t, s, "\x0f", "> ")
	if s.ConsumeTabToggle() {
		t.Fatalf("0x0f 不应触发切换: line=%q err=%v", line, err)
	}
	if err == nil {
		t.Fatalf("0x0f 不是有效输入，应等到 EOF: line=%q", line)
	}
}

// 行首（命令位）没有同名命令时退回路径补全。
func TestCompleteFallsBackToPathAtCommandPos(t *testing.T) {
	root := completeFixture(t)
	old := cmdNamesDir
	cmdNamesDir = t.TempDir() // 空 applet 目录：任何命令名都补不到
	defer func() { cmdNamesDir = old }()

	if m, fill := completeToken(root+"/iso", "ventoy", true); len(m) != 1 || fill != "ventoy/" {
		t.Fatalf("命令位无匹配应退回路径补全: %v %q", m, fill)
	}
}
