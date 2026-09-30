package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// run_command 的工作目录来自 Context.Cwd（命令模式 cd 改它），空值回退到 /。
func TestRunCommandWorkDir(t *testing.T) {
	if got := (&Context{}).WorkDir(); got != "/" {
		t.Fatalf("空 Cwd 应回退 /: %q", got)
	}
	if got := (&Context{Cwd: "/iso"}).WorkDir(); got != "/iso" {
		t.Fatalf("应使用 Cwd: %q", got)
	}
	dir := t.TempDir()
	ctx := &Context{Cwd: dir, IO: nopIO{}}
	reg := NewDefaultRegistry()
	res, err := reg.Call(ctx, "run_command", json.RawMessage(`{"cmd":"pwd"}`))
	if err != nil {
		t.Fatalf("pwd 失败: %v", err)
	}
	if !strings.Contains(res, dir) {
		t.Fatalf("pwd 应在 Cwd 下执行: %q", res)
	}
}

// nopIO 是最小的 IO 实现（run_command 在 orchestrate 下可能询问，这里只读命令不问）。
type nopIO struct{}

func (nopIO) Printf(string, ...any)                             {}
func (nopIO) Ask(string) (string, error)                        { return "", nil }
func (nopIO) Confirm(string, string) (bool, error)              { return false, nil }
func (nopIO) TypedConfirm(string, string, string) (bool, error) { return false, nil }
func (nopIO) ScriptMode() bool                                  { return false }

// find 从 shell 类黑名单移出后：普通用法按只读放行，带 -exec/-delete 等仍拒绝。
func TestFindAllowedUnlessDangerousArgs(t *testing.T) {
	if shellAllowed("orchestrate", "find") != true {
		t.Fatal("find 不应被 shell 类名单拦住")
	}
	if !isReadOnlyCommand("find", []string{"/iso", "-name", "*.iso"}) {
		t.Fatal("普通 find 应视为只读（orchestrate 下免确认）")
	}
	for _, bad := range []string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fls", "-fprint", "-fprint0", "-fprintf"} {
		args := []string{"/iso", bad, "x"}
		if !hasDangerousFindArg(args) {
			t.Fatalf("%s 应被判定为危险参数", bad)
		}
		if isReadOnlyCommand("find", args) {
			t.Fatalf("带 %s 的 find 不应视为只读", bad)
		}
	}
}

// shell 类命令只允许在 direct 模式下执行。
func TestShellAllowedOnlyInDirectMode(t *testing.T) {
	for _, mode := range []string{"readonly", "orchestrate"} {
		for _, cmd := range []string{"sh", "bash", "python3", "awk", "xargs", "timeout"} {
			if shellAllowed(mode, cmd) {
				t.Fatalf("%s 模式不应放行 %s", mode, cmd)
			}
		}
	}
	for _, cmd := range []string{"sh", "python3", "busybox"} {
		if !shellAllowed("direct", cmd) {
			t.Fatalf("direct 模式应放行 %s", cmd)
		}
	}
	if !shellAllowed("readonly", "ls") {
		t.Fatal("非 shell 类命令不应被 shellAllowed 拦住")
	}
}
