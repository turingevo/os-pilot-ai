package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 参数白名单：任何写操作的 argv 都由本包自行拼装，用户字符串只允许落在这些字符集内，
// 且一律用 exec.Command(argv) 直接执行、不经 shell。这就是“危险参数拦截”的落点：
// 调用方无法传入 -F/--delete 之外的自定义开关，也无法注入路径或元字符。
// fsSpec 声明一种文件系统：怎么建、parted 里叫什么、卷标上限、GPT 上要不要标成
// Microsoft Basic Data、以及用哪个工具检查（fsck 只写进提示，不作为工具开放）。
// 新增一种格式只加一行；挂载选项的另一份实现在 init/mount_payload.sh 的 fs_opts()。
type fsSpec struct {
	mkfsBin     string                           // 建文件系统的可执行名（裸名，靠 exec 时的 PATH 解析）
	mkfs        func(dev, label string) []string // argv 拼装（各类型差异是位置+条件，不是模板）
	partedToken string                           // parted 的 fs-type 记号；"" = 无此记号，mkpart 省略
	maxLabel    int                              // 卷标长度上限（全局 labelRe 已限 16，这里取更小的）
	msftData    bool                             // GPT 数据分区补 set N msftdata on（跨平台可见）
	fsckBin     string                           // 修复/检查工具名（仅供提示文案）
}

var fsSpecs = map[string]fsSpec{
	// -O ^orphan_file,^metadata_csum_seed：随包的 e2fsprogs 1.47 内置默认会开这两个特性，而目标发行版的
	// e2fsck/GRUB（如 Ubuntu 22.04 的 1.46.5 / GRUB 2.06）读不了带它们的 ext4 —— 表现为安装器
	// grub-install 报 "unknown filesystem"（ESP 留空）、装完开机 systemd-fsck 失败进紧急模式。
	"ext2": {mkfsBin: "mke2fs", partedToken: "ext2", maxLabel: 16, fsckBin: "e2fsck",
		mkfs: func(dev, label string) []string { return mke2fsArgv("ext2", dev, label) }},
	"ext3": {mkfsBin: "mke2fs", partedToken: "ext3", maxLabel: 16, fsckBin: "e2fsck",
		mkfs: func(dev, label string) []string { return mke2fsArgv("ext3", dev, label) }},
	"ext4": {mkfsBin: "mke2fs", partedToken: "ext4", maxLabel: 16, fsckBin: "e2fsck",
		mkfs: func(dev, label string) []string { return mke2fsArgv("ext4", dev, label) }},
	// ESP 必须是 FAT：vfat 走内置 busybox 的 mkfs.vfat（-F 32；FAT 卷标上限 11 字符）
	"vfat": {mkfsBin: "mkfs.vfat", partedToken: "fat32", maxLabel: 11, fsckBin: "",
		mkfs: func(dev, label string) []string {
			cmd := []string{"mkfs.vfat", "-F", "32"}
			if label != "" {
				cmd = append(cmd, "-n", label)
			}
			return append(cmd, dev)
		}},
	// exFAT：Windows / macOS / Android 13+ 都能读写的通用数据盘格式。
	// parted 没有 exfat 记号（3.6 的记号全集里查过），所以 mkpart 省略 fs-type，
	// 改用 msftdata 标志让 GPT 类型成为 Microsoft Basic Data。
	"exfat": {mkfsBin: "mkfs.exfat", partedToken: "", maxLabel: 15, msftData: true, fsckBin: "fsck.exfat",
		mkfs: func(dev, label string) []string {
			cmd := []string{"mkfs.exfat"}
			if label != "" {
				cmd = append(cmd, "-L", label)
			}
			return append(cmd, dev)
		}},
	// NTFS：挂载读写走内核 ntfs3，建文件系统用随包的 mkntfs（不引入 FUSE）。
	// 同理 mkpart 用 parted 的 ntfs 记号，并补 msftdata 让 Windows 认作基本数据盘。
	// -f 必给：不带它 mkntfs 会逐簇把整个卷写零（实测 1GiB 镜像 4.29s，1TB 约 70 分钟），
	// 而本工具链其它 mkfs 都只写结构、不清零。
	"ntfs": {mkfsBin: "mkntfs", partedToken: "ntfs", maxLabel: 16, msftData: true, fsckBin: "ntfsfix",
		mkfs: func(dev, label string) []string {
			cmd := []string{"mkntfs", "-f", "-F"}
			if label != "" {
				cmd = append(cmd, "-L", label)
			}
			return append(cmd, dev)
		}},
	// f2fs：Android 内部存储与 OTG 盘。parted 有 f2fs 记号，类型保持 Linux 侧。
	"f2fs": {mkfsBin: "mkfs.f2fs", partedToken: "f2fs", maxLabel: 16, fsckBin: "fsck.f2fs",
		mkfs: func(dev, label string) []string {
			cmd := []string{"mkfs.f2fs"}
			if label != "" {
				cmd = append(cmd, "-l", label)
			}
			return append(cmd, dev)
		}},
	// XFS：Linux 侧常见数据盘，内核模块随包（能读写挂载）、parted 有 xfs 记号，
	// 但 xfsprogs 没进静态工具链 → 只声明类型，不提供 mkfs（与 hfsplus 同档）。
	"xfs": {partedToken: "xfs", maxLabel: 16},
	// HFS+：macOS 旧格式盘，只能读写挂载与分区，Linux 端没有可用的 mkfs（mkhfsplus 属 Darwin）。
	"hfsplus": {partedToken: "hfs+", maxLabel: 16},
}

func mke2fsArgv(fs, dev, label string) []string {
	cmd := []string{"mke2fs", "-t", fs, "-F", "-O", "^orphan_file,^metadata_csum_seed"}
	if label != "" {
		cmd = append(cmd, "-L", label)
	}
	return append(cmd, dev)
}

var allowedTable = map[string]bool{"gpt": true, "msdos": true}

var (
	devRe      = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,16}$`)
	partNameRe = regexp.MustCompile(`^[A-Za-z0-9_. -]{1,36}$`)
	sizeRe     = regexp.MustCompile(`^([0-9]+)(?:\.[0-9]+)?(KiB|MiB|GiB|TiB|KB|MB|GB|TB|K|M|G|T|B)?$`)
)

// defaultExecPath 是 initramfs 里随包工具的固定目录集合。
// 执行 mkfs/parted 时设的 PATH 与能力探测用的是同一份，避免出现"探测说在、执行时找不到"的偏差。
// VTOY_AI_TOOL_PATH 只用于单测与离线调试（把查找目录换成 fixture，验证"工具缺席 → 不声明能力"）。
const defaultExecPath = "/bin:/sbin:/usr/bin:/usr/sbin"
const ToolPathEnv = "VTOY_AI_TOOL_PATH"

func execPathValue() string {
	if v := os.Getenv(ToolPathEnv); v != "" {
		return v
	}
	return defaultExecPath
}

func toolSearchDirs() []string { return filepath.SplitList(execPathValue()) }

// toolAvailable 判断随包工具在不在（非静态二进制在 initramfs 里根本跑不起来，这里只看名字）。
// 不做缓存：调用点只有 schema 构建与每次 format，而测试会整体换掉查找目录来验证降级。
func toolAvailable(name string) bool {
	if name == "" {
		return false
	}
	for _, d := range toolSearchDirs() {
		if st, err := os.Stat(filepath.Join(d, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// fsNames 返回表里全部类型名（按字典序，供文案与校验共用）。
func fsNames() []string {
	out := make([]string, 0, len(fsSpecs))
	for k := range fsSpecs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SupportedFS 是 partition 的 fs 提示可接受的类型（含只能挂载读写的 hfsplus）。
func SupportedFS() []string { return fsNames() }

// FormatFSAdvertised 是「能格式化」的类型：表项声明了 mkfs，且随包工具确实在盘上。
// 给模型看的清单只从这里生成，避免出现第二份硬编码列表或“先承诺再失败”。
func FormatFSAdvertised() []string {
	out := []string{}
	for _, name := range fsNames() {
		sp := fsSpecs[name]
		if sp.mkfsBin != "" && toolAvailable(sp.mkfsBin) {
			out = append(out, name)
		}
	}
	return out
}

// NonCreatableFS 是「能识别、能读写挂载，但 Linux 端没有创建工具」的类型（HFS+）。
// 单列出来是为了让模型在对话里明确拒绝"把盘格成 HFS+"，而不是含糊失败。
func NonCreatableFS() []string {
	out := []string{}
	for _, name := range fsNames() {
		if fsSpecs[name].mkfs == nil {
			out = append(out, name)
		}
	}
	return out
}

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

// validateFS 校验对外的文件系统名。清单就是 fsSpecs 的 keys，
// 新增格式只改表；这里的报错文案也随之改变。
func validateFS(fs string) error {
	if fs == "" {
		return nil
	}
	if _, ok := fsSpecs[fs]; !ok {
		return fmt.Errorf("不支持的文件系统 %q（可用: %s；vfat 用于 EFI 系统分区）",
			fs, strings.Join(fsNames(), "/"))
	}
	return nil
}

// partedFSToken 把对外的文件系统名映射为 parted 的 fs-type 记号。
// 返回 "" 表示 parted 3.6 没有这个记号（如 exfat），mkpart 须省略 fs-type。
func partedFSToken(fs string) string {
	if sp, ok := fsSpecs[fs]; ok {
		return sp.partedToken
	}
	return ""
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
		idx := strconv.Itoa(i + 1)
		// gpt: mkpart <name> [<fs>] start end ; msdos: mkpart primary [<fs>] start end
		// parted 的 fs-type 是可省略项；无对应记号时（exfat）就省略，靠下面的类型标志表达归属。
		args := []string{"parted", "-s", cdev, "mkpart", name}
		if token := partedFSToken(r.spec.FS); token != "" {
			args = append(args, token)
		}
		args = append(args, r.start, r.end)
		cmds = append(cmds, args)
		if table == "gpt" && r.spec.Name != "" {
			cmds = append(cmds, []string{"parted", "-s", cdev, "name", idx, r.spec.Name})
		}
		// GPT 分区类型 GUID：parted 默认写 Linux FS data。跨平台数据盘（exFAT/NTFS）必须
		// 标成 Microsoft Basic Data，否则 Windows 不认、macOS/Android 挂载体验差。
		// Linux 侧格式（ext*/xfs/f2fs/hfs+）不加 —— 加了会被 Windows 当作可格式化的基本数据盘。
		if table == "gpt" && fsSpecs[r.spec.FS].msftData {
			cmds = append(cmds, []string{"parted", "-s", cdev, "set", idx, "msftdata", "on"})
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
	// 单次派发：argv 拼装、卷标上限、能否创建，全部由 fsSpecs 表决定。
	sp := fsSpecs[fstype]
	if sp.mkfs == nil {
		return nil, fmt.Errorf("%s 在 Linux 端没有可用的创建工具（只能挂载读写）；跨平台数据盘请用 exfat", fstype)
	}
	if !toolAvailable(sp.mkfsBin) {
		return nil, fmt.Errorf("当前环境缺少 %s，无法格式化为 %s（可格式化: %s）",
			sp.mkfsBin, fstype, strings.Join(FormatFSAdvertised(), "/"))
	}
	if len(label) > sp.maxLabel {
		return nil, fmt.Errorf("%s 卷标最多 %d 个字符（当前 %d 个）: %q", fstype, sp.maxLabel, len(label), label)
	}
	cdev := p.canonDev(dev)
	return sp.mkfs(cdev, label), nil
}

// FormatPlan 在真正执行前给出将运行的命令。
func (p *Probe) FormatPlan(dev, fstype, label, payloadRoot string) ([]string, error) {
	cmd, err := p.buildFormat(dev, fstype, label, payloadRoot)
	if err != nil {
		return nil, err
	}
	return []string{strings.Join(cmd, " ")}, nil
}

// Format 在分区上创建文件系统；支持的类型与各自的 argv 由 fsSpecs 表决定（见表即知全貌）。
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
		// fsck 工具只在这里出现在提示里：不开放成 agent 工具，避免绕过 format 的逐字确认闸门。
		hint := ""
		if fb := fsSpecs[fstype].fsckBin; fb != "" {
			hint = "（可先用 " + fb + " 检查该分区）"
		}
		return res, fmt.Errorf("执行 %q 失败: %v%s\n%s", strings.Join(cmd, " "), err, hint, tail(out))
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
