package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFixture 造一棵假的 sysfs / /dev / /proc/mounts，并用假 Runner 记录将执行的命令。
// 关键点：真实内核里 /sys/class/block/<dev> 是指向 devices 树的符号链接，分区节点的
// realpath 父目录就是整盘名——这里照此用符号链接搭，才能覆盖 parentDisk 的反查逻辑。
// 布局：vda(8GiB)+vda1(ext4，挂在 /iso) / vdb(64MiB，空盘) / vdc(2GiB)+vdc1(ext4)
func newFixture(t *testing.T, mounts string) (*Probe, *[]string, map[string]string) {
	t.Helper()
	// 用例没自行指定工具目录时，默认铺满随包工具（否则结论会随宿主环境漂移）
	if os.Getenv(ToolPathEnv) == "" {
		withTools(t, allBundledTools...)
	}
	root := t.TempDir()
	sys := filepath.Join(root, "sys-block")
	devices := filepath.Join(root, "devices")
	dev := filepath.Join(root, "dev")
	mustMkdir(t, sys)
	mustMkdir(t, devices)
	mustMkdir(t, dev)

	link := func(name, target string) {
		if err := os.Symlink(target, filepath.Join(sys, name)); err != nil {
			t.Fatal(err)
		}
	}
	addDisk := func(name string, sectors int64, model string) {
		d := filepath.Join(devices, name)
		mustMkdir(t, d)
		write(t, filepath.Join(d, "size"), itoa(sectors))
		write(t, filepath.Join(d, "removable"), "0")
		write(t, filepath.Join(d, "ro"), "0")
		if model != "" {
			mustMkdir(t, filepath.Join(d, "device"))
			write(t, filepath.Join(d, "device", "model"), model+"\n")
		}
		write(t, filepath.Join(dev, name), "")
		link(name, "../devices/"+name)
	}
	addPart := func(disk string, num int, sectors int64) {
		name := disk + itoa(int64(num))
		d := filepath.Join(devices, disk, name)
		mustMkdir(t, d)
		write(t, filepath.Join(d, "size"), itoa(sectors))
		write(t, filepath.Join(d, "partition"), itoa(int64(num)))
		write(t, filepath.Join(dev, name), "")
		link(name, "../devices/"+disk+"/"+name)
	}

	addDisk("vda", 16777216, "FakeSSD") // 8 GiB
	addPart("vda", 1, 16777183)
	addDisk("vdb", 131072, "FakeUSB") // 64 MiB
	// 稳定标识：virtio 在 <dev>/serial；SATA/SCSI 在新内核只有 device/wwid
	write(t, filepath.Join(devices, "vdb", "serial"), "AITARGET\n")
	addDisk("vdc", 4194304, "FakeHDD") // 2 GiB
	addPart("vdc", 1, 4194271)
	write(t, filepath.Join(devices, "vdc", "device", "wwid"), "naa.5000c500abc123\n")

	mountsFile := filepath.Join(root, "mounts")
	write(t, mountsFile, mounts)

	// blkid 的键要用“文件系统命名空间”里的路径（probe 会按 DevDir 解析后再调用）
	blkid := map[string]string{
		filepath.Join(dev, "vda1"): `TYPE="ext4" LABEL="PAYLOAD" UUID="1111-2222"`,
		filepath.Join(dev, "vdc1"): `TYPE="ext4" LABEL="TARGET" UUID="3333-4444"`,
	}
	var calls []string
	p := &Probe{
		SysBlock:   sys,
		MountsFile: mountsFile,
		DevDir:     dev,
		Runner: func(name string, args ...string) (string, error) {
			calls = append(calls, strings.Join(append([]string{name}, args...), " "))
			if name == "blkid" && len(args) == 1 {
				return blkid[args[0]], nil
			}
			return "", nil
		},
	}
	return p, &calls, blkid
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

const mountsPayload = "/dev/vda1 /iso ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\n"

// 枚举：识别整盘/分区、文件系统/卷标/UUID、挂载点；承载 payload 的设备标记为受保护。
func TestListMarksPayloadProtected(t *testing.T) {
	p, _, _ := newFixture(t, mountsPayload)
	disks, err := p.List("/iso")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	if len(disks) != 3 {
		t.Fatalf("应枚举出 3 个整盘，得到 %d: %+v", len(disks), disks)
	}

	vda := byName["vda"]
	if !vda.Protected || !strings.Contains(vda.ProtectWhy, "payload") {
		t.Fatalf("vda 承载 payload，应受保护: %+v", vda)
	}
	if vda.SizeBytes != 8*1024*1024*1024 {
		t.Fatalf("vda 容量错: %d", vda.SizeBytes)
	}
	if vda.Model != "FakeSSD" || vda.Transport != "virtio" {
		t.Fatalf("vda 型号/总线错: %q %q", vda.Model, vda.Transport)
	}
	if len(vda.Partitions) != 1 {
		t.Fatalf("vda 应有 1 个分区: %+v", vda.Partitions)
	}
	part := vda.Partitions[0]
	if part.Path != "/dev/vda1" || part.Filesystem != "ext4" || part.Label != "PAYLOAD" || part.MountPoint != "/iso" {
		t.Fatalf("vda1 信息错: %+v", part)
	}
	if !part.Protected {
		t.Fatalf("vda1 已挂载，应受保护: %+v", part)
	}

	vdb := byName["vdb"]
	if vdb.Protected {
		t.Fatalf("vdb 未被挂载，不应受保护: %+v", vdb)
	}
	if vdb.Removable {
		t.Fatal("vdb 的 removable 应为 0")
	}
	if vdb.Filesystem != "" || len(vdb.Partitions) != 0 {
		t.Fatalf("vdb 应为空盘: %+v", vdb)
	}
}

// 分区表类型该从 parted print 读出（这里 runner 恒返回空串，故为空）。
func TestListPartTableFromPartedPrint(t *testing.T) {
	p, calls, _ := newFixture(t, mountsPayload)
	p.Runner = func(name string, args ...string) (string, error) {
		*calls = append(*calls, strings.Join(append([]string{name}, args...), " "))
		if name == "parted" && strings.HasSuffix(strings.Join(args, " "), "print") {
			return "Partition Table: gpt\n", nil
		}
		return "", nil
	}
	disks, _ := p.List("/iso")
	for _, d := range disks {
		if d.Name == "vdc" && d.PartTable != "gpt" {
			t.Fatalf("vdc 分区表应为 gpt: %+v", d)
		}
	}
}

// 受保护设备在分区/格式化前就必须被拒（硬守卫，与安全模式无关）。
func TestProtectedRefusals(t *testing.T) {
	p, calls, _ := newFixture(t, mountsPayload)

	if prot, why := p.Protected("/dev/vda", "/iso"); !prot || !strings.Contains(why, "payload") {
		t.Fatalf("vda 应因承载 payload 被拒: %v %q", prot, why)
	}
	if prot, why := p.Protected("/dev/vda1", "/iso"); !prot {
		t.Fatalf("vda1 已挂载，应被拒: %v %q", prot, why)
	}
	if prot, _ := p.Protected("/dev/vdb", "/iso"); prot {
		t.Fatal("vdb 未挂载，不应被拒")
	}

	// 目标盘本身没被挂载，但它的分区被挂载 → 整盘也要拒
	p2, _, _ := newFixture(t, "/dev/vdc1 /mnt/backup ext4 rw 0 0\n")
	if prot, why := p2.Protected("/dev/vdc", "/iso"); !prot || !strings.Contains(why, "分区") {
		t.Fatalf("vdc 有分区被挂载，整盘应被拒: %v %q", prot, why)
	}

	// 分区/格式化都必须先过这道守卫
	if _, err := p.Partition("/dev/vda", "gpt", []PartSpec{{Size: "rest"}}, "/iso"); err == nil {
		t.Fatal("对承载 payload 的盘分区应被拒绝")
	}
	if _, err := p.Format("/dev/vdc1", "ext4", "", "/iso"); err != nil {
		// vdc1 未挂载、非受保护，这里应能通过守卫（fake runner 不会真执行）
		t.Fatalf("vdc1 不应被守卫拒绝: %v", err)
	}
	if _, err := p.Format("/dev/vda1", "ext4", "", "/iso"); err == nil {
		t.Fatal("格式化已挂载/承载 payload 的分区应被拒绝")
	}
	if len(*calls) == 0 {
		t.Fatal("应有命令被下发")
	}
}

// 分区表/分区的命令拼装与容量折算。
func TestPartitionPlanCommands(t *testing.T) {
	p, _, _ := newFixture(t, mountsPayload)
	steps, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{
		{Size: "16MiB", FS: "ext4", Name: "boot"},
		{Size: "rest"},
	}, "/iso")
	if err != nil {
		t.Fatal(err)
	}
	d := p.canonDev("/dev/vdb")
	want := []string{
		"parted -s " + d + " mklabel gpt",
		"parted -s " + d + " mkpart boot ext4 1MiB 17MiB",
		"parted -s " + d + " name 1 boot",
		"parted -s " + d + " mkpart primary 17MiB 100%",
	}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Fatalf("命令不符:\n got %v\nwant %v", steps, want)
	}

	// 超过磁盘容量：非末块报错，末块收敛为 100%
	if _, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{
		{Size: "100MiB"}, {Size: "10MiB"},
	}, "/iso"); err == nil {
		t.Fatal("非末块超容量应报错")
	}
	steps2, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{{Size: "100MiB"}}, "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(steps2[1], "1MiB 100%") {
		t.Fatalf("末块超容量应收敛为 100%%: %v", steps2)
	}

	// vfat 提示映射为 parted 的 fat32 记号
	steps3, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{
		{Size: "16MiB", FS: "vfat", Name: "esp"},
		{Size: "rest"},
	}, "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if steps3[1] != "parted -s "+d+" mkpart esp fat32 1MiB 17MiB" {
		t.Fatalf("vfat 应映射为 parted 的 fat32: %v", steps3)
	}
}

// 参数白名单：危险/非法参数一律拦截，绝不进入 argv。
func TestArgumentInterception(t *testing.T) {
	p, _, _ := newFixture(t, mountsPayload)

	for _, fs := range []string{"fat32", "apfs", "refs", "btrfs", "ext4; rm -rf /"} {
		if _, err := p.FormatPlan("/dev/vdc1", fs, "", "/iso"); err == nil {
			t.Fatalf("不支持的文件系统应被拒: %q", fs)
		}
	}
	for _, label := range []string{"has space", "way-too-long-label-here", "a;b", `a"b`} {
		if _, err := p.FormatPlan("/dev/vdc1", "ext4", label, "/iso"); err == nil {
			t.Fatalf("非法卷标应被拒: %q", label)
		}
	}
	for _, dev := range []string{"", "vdb", "/dev/../etc/passwd", "/dev/vdb;rm", "/dev/vdb|x", "/dev/vd b"} {
		if _, err := p.FormatPlan(dev, "ext4", "", "/iso"); err == nil {
			t.Fatalf("非法设备路径应被拒: %q", dev)
		}
	}
	if _, err := p.PartitionPlan("/dev/vdb", "bsd", []PartSpec{{Size: "1MiB"}}, "/iso"); err == nil {
		t.Fatal("不支持的分区表应被拒")
	}
	if _, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{{Size: "rest"}, {Size: "1MiB"}}, "/iso"); err == nil {
		t.Fatal("rest 不在最后应被拒")
	}
	if _, err := p.PartitionPlan("/dev/vdb", "gpt", []PartSpec{{Size: "0"}}, "/iso"); err == nil {
		t.Fatal("0 大小应被拒")
	}
	if _, err := p.PartitionPlan("/dev/vdb", "gpt", nil, "/iso"); err == nil {
		t.Fatal("空分区列表应被拒")
	}
	// 传分区当整盘要拒
	if _, err := p.PartitionPlan("/dev/vdc1", "gpt", []PartSpec{{Size: "1MiB"}}, "/iso"); err == nil {
		t.Fatal("把分区当整盘应被拒")
	}
}

// 格式化：已挂载的分区必须先卸载；否则给出 mke2fs 命令。
func TestFormat(t *testing.T) {
	p, calls, _ := newFixture(t, mountsPayload)
	steps, err := p.FormatPlan("/dev/vdc1", "ext4", "BACKUP", "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if steps[0] != "mke2fs -t ext4 -F -O ^orphan_file,^metadata_csum_seed -L BACKUP "+p.canonDev("/dev/vdc1") {
		t.Fatalf("格式化命令不符: %v", steps)
	}

	res, err := p.Format("/dev/vdc1", "ext4", "BACKUP", "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Action != "format" {
		t.Fatalf("格式化结果异常: %+v", res)
	}
	if !hasCall(*calls, "mke2fs -t ext4 -F -O ^orphan_file,^metadata_csum_seed -L BACKUP "+p.canonDev("/dev/vdc1")) {
		t.Fatalf("应下发 mke2fs 命令: %v", *calls)
	}

	// vfat（ESP 场景）：mkfs.vfat -F 32；FAT 卷标上限 11 字符
	fv, err := p.FormatPlan("/dev/vdc1", "vfat", "ESP", "/iso")
	if err != nil {
		t.Fatal(err)
	}
	if fv[0] != "mkfs.vfat -F 32 -n ESP "+p.canonDev("/dev/vdc1") {
		t.Fatalf("vfat 格式化命令不符: %v", fv)
	}
	if _, err := p.Format("/dev/vdc1", "vfat", "", "/iso"); err != nil {
		t.Fatal(err)
	}
	if !hasCall(*calls, "mkfs.vfat -F 32 "+p.canonDev("/dev/vdc1")) {
		t.Fatalf("应下发 mkfs.vfat 命令: %v", *calls)
	}
	if _, err := p.FormatPlan("/dev/vdc1", "vfat", "LABEL-TOO-LONG", "/iso"); err == nil {
		t.Fatal("vfat 卷标超过 11 字符应被拒")
	}

	// 已挂载的 vda1 应被拒
	if _, err := p.Format("/dev/vda1", "ext4", "", "/iso"); err == nil {
		t.Fatal("格式化已挂载分区应被拒")
	}
}

// 备份：目标落在敏感盘（payload/根）上要拒；src/dst 互相包含要拒；--delete 要求 dst 是挂载点。
func TestBackupGuards(t *testing.T) {
	// 挂载点用临时目录，避免在宿主上创建 /mnt/xxx
	mroot := t.TempDir()
	isoDir := filepath.Join(mroot, "iso")
	bakDir := filepath.Join(mroot, "backup")
	mustMkdir(t, isoDir)
	mustMkdir(t, bakDir)
	mounts := "/dev/vda1 " + isoDir + " ext4 rw 0 0\n/dev/vdc1 " + bakDir + " ext4 rw 0 0\n"
	p, _, _ := newFixture(t, mounts)

	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	mustMkdir(t, src)
	mustMkdir(t, dst)

	// dst 是普通目录（非挂载点、非敏感盘）→ 允许
	steps, err := p.BackupPlan(src, dst, false, false, isoDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(steps[0], "rsync -a --numeric-ids") || !strings.HasSuffix(steps[0], " "+src+"/ "+dst+"/") {
		t.Fatalf("rsync 命令不符: %v", steps)
	}

	// src/dst 相同或互相包含要拒
	if _, err := p.BackupPlan(src, src, false, false, isoDir); err == nil {
		t.Fatal("src==dst 应被拒")
	}
	mustMkdir(t, filepath.Join(src, "inner"))
	if _, err := p.BackupPlan(src, filepath.Join(src, "inner"), false, false, isoDir); err == nil {
		t.Fatal("dst 在 src 之内应被拒")
	}

	// dst 不是目录要拒
	if _, err := p.BackupPlan(src, filepath.Join(root, "nope"), false, false, isoDir); err == nil {
		t.Fatal("dst 不存在应被拒")
	}

	// 目标落在 payload 所在文件系统上要拒（isoDir 即 /iso 挂载点）
	if _, err := p.BackupPlan(src, isoDir, false, false, isoDir); err == nil {
		t.Fatal("备份到 payload 所在文件系统应被拒")
	}

	// --delete 要求 dst 是挂载点：普通目录拒，挂载点（非敏感盘）放行
	if _, err := p.BackupPlan(src, dst, true, false, isoDir); err == nil {
		t.Fatal("--delete 且 dst 非挂载点应被拒")
	}
	if _, err := p.BackupPlan(src, bakDir, true, false, isoDir); err != nil {
		t.Fatalf("--delete 且 dst 为挂载点（vdc1）时应允许: %v", err)
	}
}

// 受保护判定要覆盖“payload 设备本身就是整盘裸文件系统”的情形。
func TestProtectedBarePayloadDisk(t *testing.T) {
	p, _, _ := newFixture(t, "/dev/vdb /iso exfat rw 0 0\n")
	if prot, why := p.Protected("/dev/vdb", "/iso"); !prot || !strings.Contains(why, "payload") {
		t.Fatalf("裸盘作 payload 时应受保护: %v %q", prot, why)
	}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// 稳定标识：serial 优先、wwid 兜底；都没有则为空（调用方需退回设备路径）。
func TestDiskIdentSerialAndWWID(t *testing.T) {
	p, _, _ := newFixture(t, "")
	disks, err := p.List("")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	if got := byName["vdb"].Serial; got != "AITARGET" {
		t.Fatalf("vdb 的 serial 应为 AITARGET，实际 %q", got)
	}
	if got := byName["vdc"].WWID; got != "naa.5000c500abc123" {
		t.Fatalf("vdc 的 wwid 应为 naa.5000c500abc123，实际 %q", got)
	}
	if v, kind := p.DiskIdent("/dev/vdb"); v != "AITARGET" || kind != "serial" {
		t.Fatalf("DiskIdent(/dev/vdb) = %q,%q（应为 AITARGET,serial）", v, kind)
	}
	if v, kind := p.DiskIdent("/dev/vdc"); v != "naa.5000c500abc123" || kind != "wwid" {
		t.Fatalf("DiskIdent(/dev/vdc) = %q,%q（应为 wwid）", v, kind)
	}
	if v, kind := p.DiskIdent("/dev/vda"); v != "" || kind != "" {
		t.Fatalf("vda 无标识，应返回空；实际 %q,%q", v, kind)
	}
}
