package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"os-pilot-ai/internal/disk"
)

type fakeIO struct {
	typedOK   bool
	confirmOK bool
	words     []string
}

func (f *fakeIO) Printf(string, ...any)                {}
func (f *fakeIO) Ask(string) (string, error)           { return "", nil }
func (f *fakeIO) Confirm(string, string) (bool, error) { return f.confirmOK, nil }
func (f *fakeIO) TypedConfirm(_, _, word string) (bool, error) {
	f.words = append(f.words, word)
	return f.typedOK, nil
}
func (f *fakeIO) ScriptMode() bool { return false }

// testProbe 造一个最小可用的磁盘业务层：一块 64MiB 空盘 vdb（对外 /dev/vdb），
// 外加一个挂在 /iso 的 vda1 用来验证“受保护设备”判定。
func testProbe(t *testing.T) (*disk.Probe, *[]string) {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys")
	dev := filepath.Join(root, "dev")
	devices := filepath.Join(root, "devices")
	for _, d := range []string{sys, dev, devices, filepath.Join(devices, "vdb")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeF(t, filepath.Join(devices, "vdb", "size"), "131072")
	writeF(t, filepath.Join(devices, "vdb", "removable"), "0")
	writeF(t, filepath.Join(devices, "vdb", "ro"), "0")
	if err := os.Symlink("../devices/vdb", filepath.Join(sys, "vdb")); err != nil {
		t.Fatal(err)
	}
	writeF(t, filepath.Join(dev, "vdb"), "")

	mounts := filepath.Join(root, "mounts")
	writeF(t, mounts, "/dev/vda1 /iso ext4 rw 0 0\n")

	var calls []string
	return &disk.Probe{
		SysBlock:   sys,
		MountsFile: mounts,
		DevDir:     dev,
		Runner: func(name string, args ...string) (string, error) {
			calls = append(calls, strings.Join(append([]string{name}, args...), " "))
			return "", nil
		},
	}, &calls
}

func writeF(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}

func ctxWith(p *disk.Probe, mode string, io IO) *Context {
	return &Context{PayloadDir: "/iso", Mode: mode, IO: io, Disks: p}
}

func TestRegistryExposesDiskTools(t *testing.T) {
	names := map[string]bool{}
	for _, d := range NewDefaultRegistry().Defs() {
		names[d.Function.Name] = true
	}
	for _, want := range []string{"list_disks", "partition", "format", "backup"} {
		if !names[want] {
			t.Fatalf("注册表缺少工具 %s（现有 %v）", want, names)
		}
	}
}

func TestListDisksReturnsJSON(t *testing.T) {
	p, _ := testProbe(t)
	reg := NewDefaultRegistry()
	res, err := reg.Call(ctxWith(p, "readonly", &fakeIO{}), "list_disks", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Disks []disk.Disk `json:"disks"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("list_disks 应返回可解析 JSON: %v\n%s", err, res)
	}
	if len(out.Disks) != 1 || out.Disks[0].Path != "/dev/vdb" || out.Disks[0].Protected {
		t.Fatalf("vdb 应被列为可操作的整盘: %+v", out.Disks)
	}
}

// readonly 模式下三个写工具都要拒绝，且不产生任何命令。
func TestDiskWritesRefusedInReadonly(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	for _, tc := range []struct{ name, args string }{
		{"partition", `{"disk":"/dev/vdb","partitions":[{"size":"16MiB"}]}`},
		{"format", `{"device":"/dev/vdb","fstype":"ext4"}`},
		{"backup", `{"src":"/iso","dst":"/iso"}`},
	} {
		res, err := reg.Call(ctxWith(p, "readonly", &fakeIO{}), tc.name, json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s 在 readonly 下不应报错（应返回拒绝说明）: %v", tc.name, err)
		}
		if !strings.Contains(res, "readonly") {
			t.Fatalf("%s 在 readonly 下应被拒绝，实际: %s", tc.name, res)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("readonly 下不应下发任何命令: %v", *calls)
	}
}

// orchestrate（默认）下要逐字输入盘名确认；拒绝则不下发命令。
func TestPartitionNeedsTypedConfirm(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	args := json.RawMessage(`{"disk":"/dev/vdb","partitions":[{"size":"16MiB","fs":"ext4"}]}`)

	io := &fakeIO{typedOK: false}
	res, err := reg.Call(ctxWith(p, "orchestrate", io), "partition", args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "取消") {
		t.Fatalf("拒绝确认时应返回取消: %s", res)
	}
	if len(io.words) != 1 || io.words[0] != "vdb" {
		t.Fatalf("确认词应为盘名 vdb: %v", io.words)
	}
	if len(*calls) != 0 {
		t.Fatalf("未确认不应下发命令: %v", *calls)
	}

	io2 := &fakeIO{typedOK: true}
	res, err = reg.Call(ctxWith(p, "orchestrate", io2), "partition", args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, `"action": "partition"`) || !strings.Contains(res, `"ok": true`) {
		t.Fatalf("确认后应返回结构化结果: %s", res)
	}
	if len(*calls) == 0 {
		t.Fatal("确认后应下发 parted 命令")
	}
}

// direct 模式视为已授权：不再交互确认，但硬守卫仍然生效。
func TestDirectSkipsConfirmButKeepsGuards(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	io := &fakeIO{typedOK: false}

	res, err := reg.Call(ctxWith(p, "direct", io), "format", json.RawMessage(`{"device":"/dev/vdb","fstype":"ext4","label":"BK"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, `"ok": true`) {
		t.Fatalf("direct 下应直接执行: %s", res)
	}
	if len(io.words) != 0 {
		t.Fatal("direct 下不应弹确认")
	}
	if len(*calls) == 0 {
		t.Fatal("direct 下应下发 mke2fs 命令")
	}

	// 硬守卫：格式化承载 payload 的分区（本 fixture 里 vda1 挂在 /iso）必须被拒
	if _, err := reg.Call(ctxWith(p, "direct", io), "format", json.RawMessage(`{"device":"/dev/vda1"}`)); err == nil {
		t.Fatal("direct 模式也不能格式化承载 payload 的设备")
	}
}

// vfat（ESP 场景）：orchestrate 下逐字确认后下发；format 用 mkfs.vfat，
// partition 的 vfat 提示映射为 parted 的 fat32。
func TestVFATSupportedThroughRegistry(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	io := &fakeIO{typedOK: true}

	res, err := reg.Call(ctxWith(p, "orchestrate", io), "format",
		json.RawMessage(`{"device":"/dev/vdb","fstype":"vfat","label":"ESP"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, `"ok": true`) {
		t.Fatalf("vfat 格式化应成功: %s", res)
	}
	res, err = reg.Call(ctxWith(p, "orchestrate", io), "partition",
		json.RawMessage(`{"disk":"/dev/vdb","partitions":[{"size":"16MiB","fs":"vfat","name":"esp"},{"size":"rest","fs":"ext4"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, `"ok": true`) {
		t.Fatalf("含 vfat 提示的分区应成功: %s", res)
	}
	// 命令里的设备路径指向 fixture 的临时目录，这里只比对命令尾段
	joined := strings.Join(*calls, "\n")
	for _, want := range []string{
		"mkfs.vfat -F 32 -n ESP ",
		" mkpart esp fat32 1MiB 17MiB",
		" mkpart primary ext4 17MiB 100%",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少命令 %q:\n%s", want, joined)
		}
	}
	if len(io.words) != 2 || io.words[0] != "vdb" || io.words[1] != "vdb" {
		t.Fatalf("两次写操作都应逐字确认盘名: %v", io.words)
	}
}

// 参数拦截：非法文件系统/设备/大小在确认之前就被拒。
func TestDiskArgumentInterceptionViaRegistry(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	io := &fakeIO{typedOK: true}
	cases := []struct{ name, args string }{
		{"format", `{"device":"/dev/vdb","fstype":"exfat"}`},
		{"format", `{"device":"/dev/../etc/passwd","fstype":"ext4"}`},
		{"format", `{"device":"vdb","fstype":"ext4"}`},
		{"partition", `{"disk":"/dev/vdb","partitions":[{"size":"0"}]}`},
		{"partition", `{"disk":"/dev/vdb","table":"bsd","partitions":[{"size":"16MiB"}]}`},
		{"partition", `{"disk":"/dev/vdb","partitions":[]}`},
	}
	for _, tc := range cases {
		if _, err := reg.Call(ctxWith(p, "orchestrate", io), tc.name, json.RawMessage(tc.args)); err == nil {
			t.Fatalf("%s %s 应被参数校验拒绝", tc.name, tc.args)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("参数非法时不应下发命令: %v", *calls)
	}
	if len(io.words) != 0 {
		t.Fatal("参数非法时不应进入确认环节")
	}
}

// 新工具链命令的只读归类：print/-l 免确认，写类子命令仍需确认。
func TestToolchainReadOnlyClassification(t *testing.T) {
	ro := []struct {
		cmd  string
		args []string
	}{
		{"dumpe2fs", []string{"-h", "/dev/vdb1"}},
		{"tune2fs", []string{"-l", "/dev/vdb1"}},
		{"parted", []string{"-s", "/dev/vdb", "print"}},
		{"blkid", []string{"/dev/vdb1"}},
	}
	for _, c := range ro {
		if !isReadOnlyCommand(c.cmd, c.args) {
			t.Fatalf("%s %v 应算只读", c.cmd, c.args)
		}
	}
	rw := []struct {
		cmd  string
		args []string
	}{
		{"parted", []string{"-s", "/dev/vdb", "mklabel", "gpt"}},
		{"parted", []string{"-s", "/dev/vdb", "mkpart", "primary", "1MiB", "100%"}},
		{"tune2fs", []string{"-L", "x", "/dev/vdb1"}},
		{"mke2fs", []string{"-t", "ext4", "-F", "/dev/vdb1"}},
		{"mkfs.vfat", []string{"-F", "32", "/dev/vdb1"}},
		{"rsync", []string{"-a", "/iso/", "/mnt/t/"}},
	}
	for _, c := range rw {
		if isReadOnlyCommand(c.cmd, c.args) {
			t.Fatalf("%s %v 不应算只读", c.cmd, c.args)
		}
	}
}

// backup 的 src 缺省为 payload_dir；dst 缺失时用默认值而非空串。
func TestBackupDefaultsSrcToPayload(t *testing.T) {
	p, _ := testProbe(t)
	reg := NewDefaultRegistry()
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.MkdirAll(dst, 0755); err != nil {
		t.Fatal(err)
	}
	// payload 目录不存在 → 报错里应出现 payload 路径，证明默认值生效
	_, err := reg.Call(ctxWith(p, "orchestrate", &fakeIO{confirmOK: true}), "backup",
		json.RawMessage(`{"dst":"`+dst+`"}`))
	if err == nil || !strings.Contains(err.Error(), "/iso") {
		t.Fatalf("src 缺省应为 payload_dir（/iso），实际 err=%v", err)
	}
}
