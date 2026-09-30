package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 参数白名单：任何写操作的 argv 都由本包自行拼装，用户字符串只允许落在这些字符集内，
// 且一律用 exec.Command(argv) 直接执行、不经 shell。这就是“危险参数拦截”的落点：
// 调用方无法传入 -F/--delete 之外的自定义开关，也无法注入路径或元字符。
var (
	allowedFS    = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "vfat": true}
	allowedTable = map[string]bool{"gpt": true, "msdos": true}
	// parted 的 fs-type 记号与 mkfs 名字不同：vfat 在 parted 里写作 fat32。
	partedFS   = map[string]string{"ext2": "ext2", "ext3": "ext3", "ext4": "ext4", "vfat": "fat32"}
	devRe      = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,16}$`)
	partNameRe = regexp.MustCompile(`^[A-Za-z0-9_. -]{1,36}$`)
	sizeRe     = regexp.MustCompile(`^([0-9]+)(?:\.[0-9]+)?(KiB|MiB|GiB|TiB|KB|MB|GB|TB|K|M|G|T|B)?$`)
)

const mib = int64(1024 * 1024)

// PartSpec 描述要创建的一个分区。Size 形如 "100MiB"/"512M"/"rest"/"100%"，
// "rest" 只能出现在最后一项（占用剩余空间）。FS 仅作分区类型提示（不影响格式化）。
type PartSpec struct {
	Size string `json:"size"`
	FS   string `json:"fs,omitempty"`
	Name string `json:"name,omitempty"`
}

type partRange struct {
	start, end string
	spec       PartSpec
}

// ---------- 校验 ----------

func validateDevPath(dev string) error {
	if dev == "" {
		return fmt.Errorf("设备路径不能为空")
	}
	if !strings.HasPrefix(dev, "/dev/") {
		return fmt.Errorf("设备路径必须以 /dev/ 开头: %q", dev)
	}
	if strings.Contains(dev, "..") || !devRe.MatchString(dev) {
		return fmt.Errorf("设备路径含非法字符: %q", dev)
	}
	return nil
}

func validateTable(t string) error {
	if t == "" {
		return nil // 调用方给默认值
	}
	if !allowedTable[t] {
		return fmt.Errorf("不支持的分区表 %q（可用: gpt/msdos）", t)
	}
	return nil
}

func validateFS(fs string) error {
	if fs == "" {
		return nil
	}
	if !allowedFS[fs] {
		return fmt.Errorf("不支持的文件系统 %q（可用: ext2/ext3/ext4/vfat，vfat 用于 EFI 系统分区）", fs)
	}
	return nil
}

// partedFSToken 把对外的文件系统名映射为 parted 的 fs-type 记号（vfat → fat32，其余同名）。
func partedFSToken(fs string) string {
	if t, ok := partedFS[fs]; ok {
		return t
	}
	return fs
}

func validateLabel(label string) error {
	if label == "" {
		return nil
	}
	if !labelRe.MatchString(label) {
		return fmt.Errorf("卷标 %q 非法（只允许字母/数字/._-，≤16 字符）", label)
	}
	return nil
}

// parseSize 把 "100MiB"/"10G"/"512m" 解析为字节；"rest"/"100%" 返回 rest=true。
func parseSize(s string) (bytes int64, rest bool, err error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false, fmt.Errorf("分区大小不能为空")
	}
	if t == "rest" || t == "100%" || t == "-" {
		return 0, true, nil
	}
	m := sizeRe.FindStringSubmatch(t)
	if m == nil {
		return 0, false, fmt.Errorf("无法解析分区大小 %q（示例: 100MiB / 8G / rest）", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("分区大小 %q 非法", s)
	}
	// 归一化单位：MiB/MB/M -> M，B/空 -> 字节
	u := strings.ToUpper(m[2])
	u = strings.TrimSuffix(u, "IB")
	u = strings.TrimSuffix(u, "B")
	mult := int64(1)
	switch u {
	case "K":
		mult = 1024
	case "M":
		mult = 1024 * 1024
	case "G":
		mult = 1024 * 1024 * 1024
	case "T":
		mult = 1024 * 1024 * 1024 * 1024
	}
	if n <= 0 {
		return 0, false, fmt.Errorf("分区大小必须大于 0: %q", s)
	}
	return n * mult, false, nil
}

func mibStr(b int64) string {
	return fmt.Sprintf("%dMiB", (b+mib-1)/mib)
}

// partitionPlan 把 specs 折算为 parted 的起止表达（统一从 1MiB 起，保证 4K 对齐）。
func partitionPlan(specs []PartSpec, diskBytes int64) ([]partRange, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("partitions 不能为空")
	}
	start := mib
	var out []partRange
	for i, s := range specs {
		if s.FS != "" {
			if err := validateFS(s.FS); err != nil {
				return nil, err
			}
		}
		if s.Name != "" && !partNameRe.MatchString(s.Name) {
			return nil, fmt.Errorf("分区名 %q 非法", s.Name)
		}
		bytes, rest, err := parseSize(s.Size)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个分区: %w", i+1, err)
		}
		if rest {
			if i != len(specs)-1 {
				return nil, fmt.Errorf("第 %d 个分区用了 rest/100%%，只能放在最后一项", i+1)
			}
			out = append(out, partRange{start: mibStr(start), end: "100%", spec: s})
			return out, nil
		}
		end := start + bytes
		if diskBytes > 0 && end > diskBytes {
			if i == len(specs)-1 {
				// 最后一块超出容量时收敛为占满剩余空间
				out = append(out, partRange{start: mibStr(start), end: "100%", spec: s})
				return out, nil
			}
			return nil, fmt.Errorf("第 %d 个分区超出磁盘容量（磁盘 %s）", i+1, HumanSize(diskBytes))
		}
		out = append(out, partRange{start: mibStr(start), end: mibStr(end), spec: s})
		start = end
	}
	return out, nil
}

// ---------- 受保护设备 ----------

func (p *Probe) isWholeDisk(dev string) bool {
	name := filepath.Base(dev)
	return !fileExists(filepath.Join(p.sysBlock(), name, "partition"))
}

// protectionOf 判定设备是否禁止写入，并给出原因。dev 用对外路径（/dev/xxx）。
// 判定优先级：设备本身被挂载 → 承载 payload/根的整盘 → 该设备的任一分区被挂载。
// List 与写操作的守卫共用这一份逻辑，避免两处判定漂移。
func (p *Probe) protectionOf(dev, payloadRoot string) (bool, string) {
	mounts := p.readMounts()
	payloadDev, rootDev := p.payloadAndRootDev(mounts, payloadRoot)
	clean := filepath.Clean(dev)
	base := filepath.Base(clean)

	for _, m := range mounts {
		if !sameDev(m.Dev, clean) {
			continue
		}
		switch {
		case sameDev(m.Dev, rootDev):
			return true, "承载根文件系统（挂载于 " + m.Target + "）"
		case sameDev(m.Dev, payloadDev):
			return true, "承载数据分区 payload（挂载于 " + m.Target + "）"
		default:
			return true, "已挂载于 " + m.Target
		}
	}
	for _, ref := range []struct{ d, why string }{
		{rootDev, "承载根文件系统"},
		{payloadDev, "承载数据分区 payload"},
	} {
		if ref.d == "" {
			continue
		}
		if sameDev(ref.d, clean) {
			return true, ref.why
		}
		if p.parentDiskOfPath(ref.d) == base {
			return true, ref.why + "（整盘不可动：其上有分区承载运行环境）"
		}
	}
	if p.isWholeDisk(dev) {
		for _, m := range mounts {
			if p.parentDiskOfPath(m.Dev) == base {
				return true, "有分区已挂载于 " + m.Target
			}
		}
	}
	return false, ""
}

// Protected 判断设备是否禁止写入（承载 payload / 根文件系统 / 已挂载），返回原因。
func (p *Probe) Protected(dev, payloadRoot string) (bool, string) {
	return p.protectionOf(dev, payloadRoot)
}

func (p *Probe) requireWritable(dev, payloadRoot string) error {
	if prot, why := p.Protected(dev, payloadRoot); prot {
		return fmt.Errorf("拒绝操作 %s：%s（这是运行中的系统/数据盘，禁止改分区或格式化）", dev, why)
	}
	return nil
}

// Sensitive 判断设备是否承载运行中的系统/数据（payload 或根文件系统，含同一块盘）。
// 与 Protected 的区别：备份目标本来就应该是“已挂载”的盘，所以备份只拦敏感盘，
// 不再因为“已挂载”而拒绝。
func (p *Probe) Sensitive(dev, payloadRoot string) (bool, string) {
	mounts := p.readMounts()
	payloadDev, rootDev := p.payloadAndRootDev(mounts, payloadRoot)
	clean := filepath.Clean(dev)
	disk := filepath.Base(clean)
	for _, ref := range []struct{ d, why string }{
		{rootDev, "承载根文件系统"},
		{payloadDev, "承载数据分区 payload"},
	} {
		if ref.d == "" {
			continue
		}
		if sameDev(ref.d, clean) {
			return true, ref.why
		}
		if p.parentDiskOfPath(ref.d) == disk {
			return true, ref.why + "（与该盘同属一块物理盘）"
		}
	}
	return false, ""
}

// ---------- 命令拼装 ----------

func previews(cmds [][]string) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (p *Probe) buildPartition(dev, table string, specs []PartSpec, payloadRoot string) ([][]string, error) {
	if err := validateDevPath(dev); err != nil {
		return nil, err
	}
	if err := validateTable(table); err != nil {
		return nil, err
	}
	if table == "" {
		table = "gpt"
	}
	if !fileExists(p.canonDev(dev)) {
		return nil, fmt.Errorf("设备不存在: %s", dev)
	}
	if !p.isWholeDisk(dev) {
		return nil, fmt.Errorf("%s 是分区，不是整盘；请传整盘设备（如 /dev/sdb）", dev)
	}
	if err := p.requireWritable(dev, payloadRoot); err != nil {
		return nil, err
	}
	ranges, err := partitionPlan(specs, p.diskSize(dev))
	if err != nil {
		return nil, err
	}

	cdev := p.canonDev(dev)
	cmds := [][]string{{"parted", "-s", cdev, "mklabel", table}}
	for i, r := range ranges {
		name := r.spec.Name
		if name == "" {
			name = "primary"
		}
		// gpt: mkpart <name> <fs> start end ; msdos: mkpart primary <fs> start end
		// fs 为空时省略该占位（parted 的 fs-type 可省略）；对外的 vfat 映射为 parted 的 fat32
		args := []string{"parted", "-s", cdev, "mkpart", name}
		if r.spec.FS != "" {
			args = append(args, partedFSToken(r.spec.FS))
		}
		args = append(args, r.start, r.end)
		cmds = append(cmds, args)
		if table == "gpt" && r.spec.Name != "" {
			cmds = append(cmds, []string{"parted", "-s", cdev, "name", strconv.Itoa(i + 1), r.spec.Name})
		}
	}
	return cmds, nil
}

// PartitionPlan 在真正执行前给出将运行的命令（也用于确认提示里展示“将要做什么”）。
func (p *Probe) PartitionPlan(dev, table string, specs []PartSpec, payloadRoot string) ([]string, error) {
	cmds, err := p.buildPartition(dev, table, specs, payloadRoot)
	if err != nil {
		return nil, err
	}
	return previews(cmds), nil
}

func (p *Probe) diskSize(dev string) int64 {
	return p.sizeBytes(filepath.Base(dev))
}

// ---------- 写操作 ----------

// Partition 对整盘重建分区表并创建分区。
func (p *Probe) Partition(dev, table string, specs []PartSpec, payloadRoot string) (Result, error) {
	res := Result{OK: false, Action: "partition", Target: dev}
	cmds, err := p.buildPartition(dev, table, specs, payloadRoot)
	if err != nil {
		return res, err
	}
	res.Steps = previews(cmds)
	for _, c := range cmds {
		out, err := p.runner()(c[0], c[1:]...)
		res.Output = appendLines(res.Output, out)
		if err != nil {
			return res, fmt.Errorf("执行 %q 失败: %v\n%s", strings.Join(c, " "), err, tail(out))
		}
	}
	// busybox 环境没有 udev，靠 mdev 刷新 /dev 分区节点（失败不影响结果）
	if _, err := p.runner()("mdev", "-s"); err != nil {
		res.Output = appendLines(res.Output, "（mdev -s 失败，忽略）")
	}
	res.OK = true
	res.Detail = fmt.Sprintf("已在 %s 建立 %s 分区表并创建 %d 个分区", dev, tableOrDefault(table), len(specs))
	res.Devices = p.partitionPaths(dev)
	return res, nil
}

// buildFormat 校验参数并拼出 mke2fs 命令（同时用于确认提示与实际执行）。
func (p *Probe) buildFormat(dev, fstype, label, payloadRoot string) ([]string, error) {
	if err := validateDevPath(dev); err != nil {
		return nil, err
	}
	if fstype == "" {
		fstype = "ext4"
	}
	if err := validateFS(fstype); err != nil {
		return nil, err
	}
	if err := validateLabel(label); err != nil {
		return nil, err
	}
	if !fileExists(p.canonDev(dev)) {
		return nil, fmt.Errorf("设备不存在: %s", dev)
	}
	if p.isWholeDisk(dev) {
		if parts := p.partitionPaths(dev); len(parts) > 0 {
			return nil, fmt.Errorf("%s 是整盘且有分区，请指定具体分区（如 %s1）", dev, dev)
		}
	}
	// 已挂载的分区不能格式化；若是承载运行环境的设备，直接说明原因
	if mounted := p.mountPointOf(dev); mounted != "" {
		if prot, why := p.Protected(dev, payloadRoot); prot {
			return nil, fmt.Errorf("拒绝格式化 %s：%s", dev, why)
		}
		return nil, fmt.Errorf("拒绝格式化 %s：已挂载于 %s，请先卸载后再格式化", dev, mounted)
	}
	if err := p.requireWritable(dev, payloadRoot); err != nil {
		return nil, err
	}
	// ESP 必须是 FAT：vfat 走内置 busybox 的 mkfs.vfat（-F 32；FAT 卷标上限 11 字符）。
	if fstype == "vfat" {
		if len(label) > 11 {
			return nil, fmt.Errorf("vfat 卷标最多 11 个字符（当前 %d 个）: %q", len(label), label)
		}
		cmd := []string{"mkfs.vfat", "-F", "32"}
		if label != "" {
			cmd = append(cmd, "-n", label)
		}
		return append(cmd, p.canonDev(dev)), nil
	}
	// -O ^orphan_file,^metadata_csum_seed：随包的 e2fsprogs 1.47 内置默认会开这两个特性，而目标发行版的
	// e2fsck/GRUB（如 Ubuntu 22.04 的 1.46.5 / GRUB 2.06）读不了带它们的 ext4 —— 表现为安装器
	// grub-install 报 "unknown filesystem"（ESP 留空）、装完开机 systemd-fsck 失败进紧急模式。
	cmd := []string{"mke2fs", "-t", fstype, "-F", "-O", "^orphan_file,^metadata_csum_seed"}
	if label != "" {
		cmd = append(cmd, "-L", label)
	}
	return append(cmd, p.canonDev(dev)), nil
}

// FormatPlan 在真正执行前给出将运行的命令。
func (p *Probe) FormatPlan(dev, fstype, label, payloadRoot string) ([]string, error) {
	cmd, err := p.buildFormat(dev, fstype, label, payloadRoot)
	if err != nil {
		return nil, err
	}
	return []string{strings.Join(cmd, " ")}, nil
}

// Format 在分区上创建文件系统（ext2/ext3/ext4 用内置 e2fsprogs；vfat 用内置 mkfs.vfat，供 ESP 使用）。
func (p *Probe) Format(dev, fstype, label, payloadRoot string) (Result, error) {
	res := Result{OK: false, Action: "format", Target: dev}
	cmd, err := p.buildFormat(dev, fstype, label, payloadRoot)
	if err != nil {
		return res, err
	}
	if fstype == "" {
		fstype = "ext4"
	}
	res.Steps = []string{strings.Join(cmd, " ")}
	out, err := p.runner()(cmd[0], cmd[1:]...)
	res.Output = appendLines(res.Output, out)
	if err != nil {
		return res, fmt.Errorf("执行 %q 失败: %v\n%s", strings.Join(cmd, " "), err, tail(out))
	}
	res.OK = true
	res.Detail = fmt.Sprintf("已在 %s 创建 %s 文件系统", dev, fstype)
	if label != "" {
		res.Detail += "，卷标 " + label
	}
	var fs, lb, uuid, mp string
	p.FillFS(&fs, &lb, &uuid, &mp, filepath.Base(dev))
	if uuid != "" {
		res.Detail += "，UUID " + uuid
	}
	return res, nil
}

// buildBackup 校验参数并拼出 rsync 命令。
func (p *Probe) buildBackup(src, dst string, del, dryRun bool, payloadRoot string) ([]string, string, string, error) {
	if strings.TrimSpace(src) == "" || strings.TrimSpace(dst) == "" {
		return nil, "", "", fmt.Errorf("src 与 dst 都不能为空")
	}
	sa, err := filepath.Abs(src)
	if err != nil {
		return nil, "", "", err
	}
	da, err := filepath.Abs(dst)
	if err != nil {
		return nil, "", "", err
	}
	sa, da = filepath.Clean(sa), filepath.Clean(da)
	for _, d := range []struct{ p, what string }{{sa, "src"}, {da, "dst"}} {
		st, err := os.Stat(d.p)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s 不存在: %s", d.what, d.p)
		}
		if !st.IsDir() {
			return nil, "", "", fmt.Errorf("%s 不是目录: %s", d.what, d.p)
		}
	}
	if sa == da {
		return nil, "", "", fmt.Errorf("src 与 dst 是同一个目录: %s", sa)
	}
	if isUnder(da, sa) || isUnder(sa, da) {
		return nil, "", "", fmt.Errorf("src 与 dst 不能互相包含（%s / %s）", sa, da)
	}
	if dev, target := p.mountForDir(da); dev != "" {
		if sens, why := p.Sensitive(dev, payloadRoot); sens {
			return nil, "", "", fmt.Errorf("拒绝写入 %s：目标在 %s 上，%s", da, target, why)
		}
	}
	if del && !p.isMountPoint(da) {
		return nil, "", "", fmt.Errorf("--delete 模式下 dst 必须是一个挂载点（避免误删共享目录）: %s", da)
	}
	cmd := []string{"rsync", "-a", "--numeric-ids", "--human-readable", "--stats"}
	if del {
		cmd = append(cmd, "--delete")
	}
	if dryRun {
		cmd = append(cmd, "--dry-run")
	}
	cmd = append(cmd, sa+"/", da+"/")
	return cmd, sa, da, nil
}

// BackupPlan 在真正执行前给出将运行的命令。
func (p *Probe) BackupPlan(src, dst string, del, dryRun bool, payloadRoot string) ([]string, error) {
	cmd, _, _, err := p.buildBackup(src, dst, del, dryRun, payloadRoot)
	if err != nil {
		return nil, err
	}
	return []string{strings.Join(cmd, " ")}, nil
}

// Backup 用 rsync 把 src 目录内容同步到 dst 目录。禁止写向运行中的系统/数据盘；
// delete=true 时要求 dst 本身是一个挂载点（避免 --delete 误删共享目录）。
func (p *Probe) Backup(src, dst string, del, dryRun bool, payloadRoot string) (Result, error) {
	res := Result{OK: false, Action: "backup", Target: dst}
	cmd, sa, da, err := p.buildBackup(src, dst, del, dryRun, payloadRoot)
	if err != nil {
		return res, err
	}
	res.Target = da
	res.Steps = []string{strings.Join(cmd, " ")}
	out, err := p.runner()(cmd[0], cmd[1:]...)
	res.Output = appendLines(res.Output, out)
	if err != nil {
		return res, fmt.Errorf("rsync 失败: %v\n%s", err, tail(out))
	}
	res.OK = true
	res.Detail = fmt.Sprintf("已把 %s 的内容同步到 %s", sa, da)
	if dryRun {
		res.Detail = "（试运行）" + res.Detail
	}
	return res, nil
}

// ---------- 辅助 ----------

func tableOrDefault(t string) string {
	if t == "" {
		return "gpt"
	}
	return t
}

func (p *Probe) partitionPaths(disk string) []string {
	entries, err := os.ReadDir(p.sysBlock())
	if err != nil {
		return nil
	}
	want := filepath.Base(disk)
	var nums []int
	for _, e := range entries {
		name := e.Name()
		if !fileExists(filepath.Join(p.sysBlock(), name, "partition")) {
			continue
		}
		if p.parentDisk(name) != want {
			continue
		}
		n, _ := strconv.Atoi(readTrim(filepath.Join(p.sysBlock(), name, "partition")))
		nums = append(nums, n)
	}
	sortInts(nums)
	out := make([]string, 0, len(nums))
	for _, n := range nums {
		out = append(out, p.pubDev(p.partName(want, n)))
	}
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

func (p *Probe) mountPointOf(dev string) string {
	for _, m := range p.readMounts() {
		if sameDev(m.Dev, dev) {
			return m.Target
		}
	}
	return ""
}

// mountForDir 返回包含 dir 的最内层挂载（设备, 挂载点）。
func (p *Probe) mountForDir(dir string) (string, string) {
	bestLen, bestDev, bestTarget := -1, "", ""
	for _, m := range p.readMounts() {
		if dir == m.Target || strings.HasPrefix(dir, m.Target+"/") {
			if len(m.Target) > bestLen {
				bestLen, bestDev, bestTarget = len(m.Target), m.Dev, m.Target
			}
		}
	}
	return bestDev, bestTarget
}

func (p *Probe) isMountPoint(dir string) bool {
	for _, m := range p.readMounts() {
		if m.Target == dir {
			return true
		}
	}
	return false
}

func isUnder(child, parent string) bool {
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

func appendLines(dst []string, out string) []string {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		dst = append(dst, ln)
	}
	if len(dst) > 80 {
		dst = append(dst[:80], "…（输出已截断）")
	}
	return dst
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2048 {
		return s[len(s)-2048:]
	}
	return s
}
