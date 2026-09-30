package disk

import (
	"strings"
	"testing"
)

func testParts() []AutoinstallPart {
	return []AutoinstallPart{
		{Size: "512M", Label: "boot-efi", Mount: "/boot/efi", Boot: true},
		{Size: "8G", Label: "root", Mount: "/"},
		{Size: "rest", Label: "home", Mount: "/home"},
	}
}

// 选盘必须用 serial、字段名必须是 curtin schema（format 用 volume、mount 用 device+path），
// 且整个模板里不能出现 /dev/ 设备名。
func TestAutoinstallTemplateUsesSerialNotDevicePath(t *testing.T) {
	out, err := AutoinstallTemplate(AutoinstallOptions{
		Image:     "/ISO/ubuntu-24.04.iso",
		Disk:      "/dev/vdb",
		Ident:     "AITARGET",
		IdentKind: "serial",
		Parts:     testParts(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`serial: "AITARGET"`,
		"type: disk",
		"type: partition, device: disk0",
		"type: format, volume: part1",
		"type: format, volume: part2",
		"type: format, volume: part3",
		"type: mount, device: part2-fmt, path: /",
		"type: mount, device: part1-fmt, path: /boot/efi",
		"flag: boot",
		"fstype: vfat",
		`size: -1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("模板缺少 %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "/dev/") {
		t.Errorf("模板不应出现设备名 /dev/：\n%s", out)
	}
	if strings.Contains(out, "fstab_id") || strings.Contains(out, "type: fs") {
		t.Errorf("模板含非法字段（fstab_id / type: fs）：\n%s", out)
	}
}

// 没有 serial 时应退回设备路径，并让调用方知道（IdentKind 非 serial）。
func TestAutoinstallTemplateFallsBackToPath(t *testing.T) {
	out, err := AutoinstallTemplate(AutoinstallOptions{
		Image:     "/ISO/x.iso",
		Disk:      "/dev/sdb",
		IdentKind: "path",
		Parts:     []AutoinstallPart{{Size: "rest", Label: "root", Mount: "/"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "path: /dev/sdb") {
		t.Fatalf("应退回 path 选盘:\n%s", out)
	}
	if strings.Contains(out, "serial:") {
		t.Fatalf("无 serial 时不应写 serial:\n%s", out)
	}
}

func TestAutoinstallTemplateRejects(t *testing.T) {
	cases := []struct {
		name string
		opt  AutoinstallOptions
		want string
	}{
		{"无根分区", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "8G", Label: "home", Mount: "/home"}}}, "必须有一个分区挂载到 /"},
		{"rest 不在最后", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "rest", Label: "a", Mount: "/"}, {Size: "1G", Label: "b", Mount: "/home"}}}, "只能放在最后"},
		{"卷标缺失", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "rest", Mount: "/"}}}, "卷标必须给"},
		{"挂载点重复", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "8G", Label: "r", Mount: "/"}, {Size: "rest", Label: "h", Mount: "/"}}}, "挂载点重复"},
		{"文件系统不支持", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "rest", Label: "r", Mount: "/", FS: "ntfs"}}}, "不支持的文件系统"},
		{"镜像非绝对路径", AutoinstallOptions{Image: "ISO/x.iso", Disk: "/dev/sdb", Parts: []AutoinstallPart{{Size: "rest", Label: "r", Mount: "/"}}}, "绝对路径"},
		{"分区为空", AutoinstallOptions{Image: "/ISO/x.iso", Disk: "/dev/sdb"}, "至少需要一个分区"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := AutoinstallTemplate(c.opt); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望错误含 %q，实际 %v", c.want, err)
			}
		})
	}
}

// fstype 缺省推断：/boot/efi → vfat，其余 ext4。
func TestAutoinstallTemplateInfersFS(t *testing.T) {
	out, err := AutoinstallTemplate(AutoinstallOptions{
		Image: "/ISO/x.iso", Disk: "/dev/sdb", Ident: "S", IdentKind: "serial",
		Parts: []AutoinstallPart{
			{Size: "512M", Label: "efi", Mount: "/boot/efi"},
			{Size: "rest", Label: "root", Mount: "/"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "volume: part1, fstype: vfat") {
		t.Errorf("EFI 分区应推断为 vfat:\n%s", out)
	}
	if !strings.Contains(out, "volume: part2, fstype: ext4") {
		t.Errorf("根分区应推断为 ext4:\n%s", out)
	}
}
