package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allBundledTools 是"随包环境里应有"的工具名，newFixture 默认铺满它们，
// 使单测结论不受宿主装了什么影响。
var allBundledTools = []string{
	"mke2fs", "e2fsck", "resize2fs", "tune2fs", "dumpe2fs",
	"parted", "rsync", "blkid", "mkfs.vfat",
	"mkfs.exfat", "fsck.exfat", "mkntfs", "ntfsfix", "mkfs.f2fs", "fsck.f2fs",
}

// withTools 把工具查找目录（VTOY_AI_TOOL_PATH）换成只含 names 的临时目录，
// 让"随包工具在不在"这件事在单测里确定下来，而不是依赖宿主装了什么。
func withTools(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(ToolPathEnv, dir)
}

// 一张表覆盖全部类型：argv 逐字、卷标上限、parted 记号、GPT 类型标志。
// 新增格式只需要在这里加一行，本文件即是"支持矩阵"的可执行版本。
func TestFormatArgvPerFS(t *testing.T) {
	withTools(t, "mke2fs", "mkfs.vfat", "mkfs.exfat", "mkntfs", "mkfs.f2fs")
	p, _, _ := newFixture(t, mountsPayload)
	const dev = "/dev/vdc1"

	cases := []struct {
		fs    string
		label string
		want  string // FormatPlan 拼出的完整命令（设备路径按 fixture 解析，只比前缀）
	}{
		{"ext4", "", "mke2fs -t ext4 -F -O ^orphan_file,^metadata_csum_seed "},
		{"ext3", "DATA", "mke2fs -t ext3 -F -O ^orphan_file,^metadata_csum_seed -L DATA "},
		{"ext2", "", "mke2fs -t ext2 -F -O ^orphan_file,^metadata_csum_seed "},
		{"vfat", "ESP", "mkfs.vfat -F 32 -n ESP "},
		{"exfat", "SHARE", "mkfs.exfat -L SHARE "},
		{"ntfs", "WIN", "mkntfs -f -F -L WIN "},
		{"f2fs", "ANDROID", "mkfs.f2fs -l ANDROID "},
		{"", "", "mke2fs -t ext4 -F -O ^orphan_file,^metadata_csum_seed "}, // 默认 ext4
	}
	for _, tc := range cases {
		steps, err := p.FormatPlan(dev, tc.fs, tc.label, "/iso")
		if err != nil {
			t.Fatalf("fstype=%q: %v", tc.fs, err)
		}
		if len(steps) != 1 || !strings.HasPrefix(steps[0], tc.want) {
			t.Fatalf("fstype=%q 命令不符:\n got %v\nwant 前缀 %q", tc.fs, steps, tc.want)
		}
	}
}

// 卷标上限按类型收紧（全局 labelRe 只限 16）。
func TestFormatLabelLimitPerFS(t *testing.T) {
	withTools(t, "mke2fs", "mkfs.vfat", "mkfs.exfat", "mkntfs", "mkfs.f2fs")
	p, _, _ := newFixture(t, mountsPayload)

	label := func(n int) string { return "L" + strings.Repeat("x", n-1) }
	limits := map[string]int{"vfat": 11, "exfat": 15, "ext4": 16, "ntfs": 16, "f2fs": 16}
	for fs, max := range limits {
		if _, err := p.FormatPlan("/dev/vdc1", fs, label(max), "/iso"); err != nil {
			t.Fatalf("%s 卷标 %d 字符（上限内）应通过: %v", fs, max, err)
		}
		if max < 16 {
			if _, err := p.FormatPlan("/dev/vdc1", fs, label(max+1), "/iso"); err == nil {
				t.Fatalf("%s 卷标 %d 字符超出上限应被拒", fs, max+1)
			}
		}
	}
}

// hfsplus/xfs 只能读写挂载与分区：格式化诉求要在确认闸门之前就拒绝，且给出替代方案。
func TestFormatRejectsUncreatableFS(t *testing.T) {
	withTools(t, "mke2fs", "mkfs.vfat", "mkfs.exfat", "mkntfs", "mkfs.f2fs")
	p, _, _ := newFixture(t, mountsPayload)
	for _, fs := range []string{"hfsplus", "xfs"} {
		_, err := p.FormatPlan("/dev/vdc1", fs, "", "/iso")
		if err == nil {
			t.Fatalf("%s 无 Linux 端创建工具，应被拒", fs)
		}
		if !strings.Contains(err.Error(), "exfat") {
			t.Fatalf("%s 的报错应指向跨平台替代方案 exfat: %v", fs, err)
		}
	}
	// 但在分区里作为类型提示是合法的（parted 有 hfs+ / xfs 记号）
	if _, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{{Size: "rest", FS: "hfsplus"}}, "/iso"); err != nil {
		t.Fatalf("hfsplus 作为分区类型提示应通过: %v", err)
	}
}

// 能力声明完全由探测驱动：工具缺席的类型既不出现在清单里，也不能真的执行。
func TestAdvertisedFSFollowsInstalledTools(t *testing.T) {
	withTools(t, "mke2fs", "mkfs.vfat")
	if got := strings.Join(FormatFSAdvertised(), ","); got != "ext2,ext3,ext4,vfat" {
		t.Fatalf("只装 e2fsprogs+mkfs.vfat 时可格式化清单应收窄: %q", got)
	}
	// hfsplus/xfs 无 mkfs，任何情况下都不该出现在可格式化清单
	for _, fs := range FormatFSAdvertised() {
		if fs == "hfsplus" || fs == "xfs" {
			t.Fatalf("不可创建的类型进入了清单: %s", fs)
		}
	}
	if got := strings.Join(NonCreatableFS(), ","); got != "hfsplus,xfs" {
		t.Fatalf("不可创建清单不符: %q", got)
	}
	p, _, _ := newFixture(t, mountsPayload)
	if _, err := p.FormatPlan("/dev/vdc1", "exfat", "", "/iso"); err == nil {
		t.Fatal("mkfs.exfat 缺席时格式化 exfat 应报缺工具")
	}

	withTools(t) // 一个工具都没有
	if len(FormatFSAdvertised()) != 0 {
		t.Fatalf("无随包工具时可格式化清单应为空: %v", FormatFSAdvertised())
	}
	if len(SupportedFS()) == 0 {
		t.Fatal("挂载/分区能力不依赖 mkfs，清单不应为空")
	}
}

// GPT 分区类型：parted 无记号的（exfat）省略 fs-type 并补 msftdata；
// Linux 侧格式补了 msftdata 会被 Windows 当基本数据盘提示改写，所以不能加。
func TestPartitionGPTTypeRules(t *testing.T) {
	p, _, _ := newFixture(t, mountsPayload)

	cases := []struct {
		fs         string
		wantMkpart string
		wantSet    bool // gpt 上是否应追加 set N msftdata on
	}{
		{"exfat", "mkpart primary 1MiB 100%", true},     // 无记号 → 省略 fs-type，靠 msftdata 表归属
		{"ntfs", "mkpart primary ntfs 1MiB 100%", true}, // 有记号，Windows 数据盘仍需 msftdata
		{"ext4", "mkpart primary ext4 1MiB 100%", false},
		{"f2fs", "mkpart primary f2fs 1MiB 100%", false},
		{"hfsplus", "mkpart primary hfs+ 1MiB 100%", false},
		{"xfs", "mkpart primary xfs 1MiB 100%", false},
		{"vfat", "mkpart primary fat32 1MiB 100%", false},
	}
	for _, tc := range cases {
		steps, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{{Size: "rest", FS: tc.fs}}, "/iso")
		if err != nil {
			t.Fatalf("fs=%q: %v", tc.fs, err)
		}
		joined := strings.Join(steps, "\n")
		if !strings.Contains(joined, tc.wantMkpart) {
			t.Fatalf("fs=%q mkpart 不符:\n%s", tc.fs, joined)
		}
		if hasSet := strings.Contains(joined, "set 1 msftdata on"); hasSet != tc.wantSet {
			t.Fatalf("fs=%q 的 msftdata 标志不符（期望 %v）:\n%s", tc.fs, tc.wantSet, joined)
		}
	}

	// msdos 分区表没有 GPT 类型概念，任何格式都不应出现 set msftdata
	steps, err := p.PartitionPlan("/dev/vdb", "msdos", []PartSpec{{Size: "rest", FS: "exfat"}}, "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(steps, "\n"); strings.Contains(j, "msftdata") {
		t.Fatalf("msdos 不应出现 msftdata:\n%s", j)
	}
}
