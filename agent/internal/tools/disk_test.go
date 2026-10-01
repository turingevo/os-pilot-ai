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
	withTools(t, bundledTools...)
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

func withTools(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(disk.ToolPathEnv, dir)
}

// bundledTools 与随包 initramfs 的 /bin 对齐（pack_env.sh 的 TOOLS_LIST + busybox applet）。
var bundledTools = []string{
	"mke2fs", "e2fsck", "resize2fs", "tune2fs", "dumpe2fs",
	"parted", "rsync", "blkid", "mkfs.vfat",
	"mkfs.exfat", "fsck.exfat", "mkfs.f2fs", "fsck.f2fs",
	"mkntfs", "ntfsfix",
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

// 跨平台数据盘（exFAT / NTFS / f2fs）在对话链路里可用：format 走各自 mkfs，
// partition 的 GPT 类型按 msftdata 规则处理，每步仍需逐字确认盘名。
func TestCrossPlatformFSThroughRegistry(t *testing.T) {
	cases := []struct {
		fs         string
		label      string
		wantMkfs   string
		wantMkpart string
		wantSet    bool
	}{
		{"exfat", "SHARE", "mkfs.exfat -L SHARE ", "mkpart data 1MiB 100%", true},
		{"ntfs", "WIN", "mkntfs -f -F -L WIN ", "mkpart data ntfs 1MiB 100%", true},
		{"f2fs", "ANDROID", "mkfs.f2fs -l ANDROID ", "mkpart data f2fs 1MiB 100%", false},
	}
	for _, tc := range cases {
		t.Run(tc.fs, func(t *testing.T) {
			p, calls := testProbe(t)
			reg := NewDefaultRegistry()
			io := &fakeIO{typedOK: true}
			ctx := ctxWith(p, "orchestrate", io)

			res, err := reg.Call(ctx, "format", json.RawMessage(
				`{"device":"/dev/vdb","fstype":"`+tc.fs+`","label":"`+tc.label+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res, `"ok": true`) {
				t.Fatalf("%s 格式化应成功: %s", tc.fs, res)
			}
			res, err = reg.Call(ctx, "partition", json.RawMessage(
				`{"disk":"/dev/vdb","partitions":[{"size":"rest","fs":"`+tc.fs+`","name":"data"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res, `"ok": true`) {
				t.Fatalf("含 %s 提示的分区应成功: %s", tc.fs, res)
			}
			joined := strings.Join(*calls, "\n")
			for _, want := range []string{tc.wantMkfs, tc.wantMkpart} {
				if !strings.Contains(joined, want) {
					t.Fatalf("缺少命令 %q:\n%s", want, joined)
				}
			}
			if has := strings.Contains(joined, "set 1 msftdata on"); has != tc.wantSet {
				t.Fatalf("%s 的 msftdata 期望 %v，实际 %v:\n%s", tc.fs, tc.wantSet, has, joined)
			}
			if len(io.words) != 2 || io.words[0] != "vdb" {
				t.Fatalf("%s 的两次写操作都应逐字确认盘名: %v", tc.fs, io.words)
			}
		})
	}
}

// 能力声明只来自运行时探测：enum 是给模型的契约，工具缺席就从契约里消失，不会“先承诺再失败”。
func TestRegistryAdvertisesOnlyInstalledTools(t *testing.T) {
	p, calls := testProbe(t)
	enumOf := func(t *testing.T, reg *Registry, tool, arg string) []string {
		t.Helper()
		for _, d := range reg.Defs() {
			if d.Function.Name != tool {
				continue
			}
			props := d.Function.Parameters.(map[string]any)["properties"].(map[string]any)
			if arg == "fs" { // partition 的 fs 在 partitions.items.properties 里
				items := props["partitions"].(map[string]any)["items"].(map[string]any)
				props = items["properties"].(map[string]any)
			}
			enum, _ := props[arg].(map[string]any)["enum"].([]string)
			return enum
		}
		t.Fatalf("没有工具 %s", tool)
		return nil
	}

	formatFS := enumOf(t, NewDefaultRegistry(), "format", "fstype")
	for _, want := range []string{"ext4", "vfat", "exfat", "f2fs", "ntfs"} {
		if !hasStr(formatFS, want) {
			t.Fatalf("已装的 %s 应出现在 fstype enum 里: %v", want, formatFS)
		}
	}
	// hfsplus/xfs 表里就没有创建工具（Linux 端无 mkfs / xfsprogs 未随包），永远不进 enum
	if hasStr(formatFS, "hfsplus") || hasStr(formatFS, "xfs") {
		t.Fatalf("无可用的创建工具，不应进入可格式化 enum: %v", formatFS)
	}
	// 挂载与分区能力不依赖 mkfs：hfsplus/xfs 只出现在 partition 的类型提示里
	partFS := enumOf(t, NewDefaultRegistry(), "partition", "fs")
	for _, want := range []string{"hfsplus", "xfs", "exfat", "f2fs"} {
		if !hasStr(partFS, want) {
			t.Fatalf("%s 应可作为分区类型提示: %v", want, partFS)
		}
	}

	// 只装了 e2fsprogs + busybox 的 mkfs.vfat（旧构建产物目录）：跨平台类型从 enum 里消失
	withTools(t, "mke2fs", "mkfs.vfat")
	part := NewDefaultRegistry()
	got := enumOf(t, part, "format", "fstype")
	for _, want := range []string{"ext4", "vfat"} {
		if !hasStr(got, want) {
			t.Fatalf("e2fsprogs/vfat 在装时应可格式化: %v", got)
		}
	}
	for _, no := range []string{"exfat", "f2fs", "ntfs"} {
		if hasStr(got, no) {
			t.Fatalf("缺少对应 mkfs 时 %s 不应可格式化: %v", no, got)
		}
	}
	io := &fakeIO{typedOK: true}
	if _, err := part.Call(ctxWith(p, "orchestrate", io), "format",
		json.RawMessage(`{"device":"/dev/vdb","fstype":"exfat"}`)); err == nil {
		t.Fatal("未随包 mkfs.exfat 时格式化应被拒")
	}
	if len(*calls) != 0 || len(io.words) != 0 {
		t.Fatalf("部分工具缺席同样应既不执行也不确认: calls=%v words=%v", *calls, io.words)
	}

	// 一个 mkfs 都没有：enum 清空，format 在确认环节之前就被拒
	withTools(t)
	none := NewDefaultRegistry()
	if got := enumOf(t, none, "format", "fstype"); len(got) != 0 {
		t.Fatalf("工具缺席时 fstype enum 应为空: %v", got)
	}
	io = &fakeIO{typedOK: true}
	if _, err := none.Call(ctxWith(p, "orchestrate", io), "format",
		json.RawMessage(`{"device":"/dev/vdb","fstype":"exfat"}`)); err == nil {
		t.Fatal("无随包工具时格式化应被拒")
	}
	if len(*calls) != 0 || len(io.words) != 0 {
		t.Fatalf("应既不下发命令也不进入确认: calls=%v words=%v", *calls, io.words)
	}
}

func hasStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// 参数拦截：非法文件系统/设备/大小在确认之前就被拒。
func TestDiskArgumentInterceptionViaRegistry(t *testing.T) {
	p, calls := testProbe(t)
	reg := NewDefaultRegistry()
	io := &fakeIO{typedOK: true}
	cases := []struct{ name, args string }{
		{"format", `{"device":"/dev/vdb","fstype":"apfs"}`},
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
