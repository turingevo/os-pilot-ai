// Package disk 是分区/格式化/备份的业务层：枚举块设备、判定“受保护设备”、执行
// parted/mkfs/rsync。上层（agent 工具、将来的 TUI/GTK/WebUI）只依赖本包的类型与函数，
// 返回值一律是可 JSON 序列化的结构体，便于各前端复用同一业务层。
package disk

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---------- 结果类型（结构化，供 LLM 与各前端共用）----------

type Partition struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Number     int    `json:"number"`
	SizeBytes  int64  `json:"size_bytes"`
	SizeHuman  string `json:"size_human"`
	Filesystem string `json:"filesystem,omitempty"`
	Label      string `json:"label,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`
	Protected  bool   `json:"protected"`
	ProtectWhy string `json:"protect_reason,omitempty"`
}

type Disk struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	SizeBytes  int64       `json:"size_bytes"`
	SizeHuman  string      `json:"size_human"`
	Model      string      `json:"model,omitempty"`
	Transport  string      `json:"transport,omitempty"`
	Serial     string      `json:"serial,omitempty"`
	WWID       string      `json:"wwid,omitempty"`
	Removable  bool        `json:"removable"`
	ReadOnly   bool        `json:"read_only"`
	PartTable  string      `json:"partition_table,omitempty"`
	Filesystem string      `json:"filesystem,omitempty"`
	Label      string      `json:"label,omitempty"`
	UUID       string      `json:"uuid,omitempty"`
	MountPoint string      `json:"mount_point,omitempty"`
	Protected  bool        `json:"protected"`
	ProtectWhy string      `json:"protect_reason,omitempty"`
	Partitions []Partition `json:"partitions"`
}

// Result 是写操作（partition/format/backup）的统一返回。
type Result struct {
	OK      bool     `json:"ok"`
	Action  string   `json:"action"`
	Target  string   `json:"target"`
	Detail  string   `json:"detail,omitempty"`
	Steps   []string `json:"steps,omitempty"`
	Devices []string `json:"devices,omitempty"`
	Output  []string `json:"output,omitempty"`
}

// ---------- 探测环境（可注入，便于单测）----------

type Runner func(name string, args ...string) (string, error)

type Probe struct {
	SysBlock   string // 默认 /sys/class/block
	MountsFile string // 默认 /proc/mounts
	DevDir     string // 默认 /dev
	Runner     Runner
}

func DefaultProbe() *Probe {
	return &Probe{
		SysBlock:   "/sys/class/block",
		MountsFile: "/proc/mounts",
		DevDir:     "/dev",
		Runner:     execRun,
	}
}

func execRun(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Env = []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin"}
	out, err := c.CombinedOutput()
	return string(out), err
}

func (p *Probe) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return execRun
}

func (p *Probe) sysBlock() string {
	if p.SysBlock != "" {
		return p.SysBlock
	}
	return "/sys/class/block"
}

func (p *Probe) mountsFile() string {
	if p.MountsFile != "" {
		return p.MountsFile
	}
	return "/proc/mounts"
}

func (p *Probe) devDir() string {
	if p.DevDir != "" {
		return p.DevDir
	}
	return "/dev"
}

// devPath 返回设备在“文件系统命名空间”里的路径（DevDir 可注入，便于单测）。
func (p *Probe) devPath(name string) string {
	return filepath.Join(p.devDir(), filepath.Base(name))
}

// pubDev 返回设备对外的规范路径（/dev/<name>）：这是用户与工具之间传递的形式。
func (p *Probe) pubDev(name string) string {
	return filepath.Join("/dev", filepath.Base(name))
}

// canonDev 把对外路径（/dev/vdb）映射到文件系统命名空间（生产环境两者相同）。
func (p *Probe) canonDev(dev string) string {
	return filepath.Join(p.devDir(), filepath.Base(dev))
}

// ---------- 挂载表与“受保护设备” ----------

type mountEntry struct {
	Dev    string
	Target string
	FSType string
}

// readMounts 只保留真实文件系统的挂载（过滤 proc/sysfs/tmpfs 等伪文件系统）。
func (p *Probe) readMounts() []mountEntry {
	b, err := os.ReadFile(p.mountsFile())
	if err != nil {
		return nil
	}
	var out []mountEntry
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || pseudoFs[f[2]] {
			continue
		}
		out = append(out, mountEntry{Dev: unescapeMount(f[0]), Target: unescapeMount(f[1]), FSType: f[2]})
	}
	return out
}

var pseudoFs = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"securityfs": true, "cgroup": true, "cgroup2": true, "pstore": true, "bpf": true,
	"tracefs": true, "debugfs": true, "configfs": true, "fusectl": true, "mqueue": true,
	"hugetlbfs": true, "ramfs": true, "autofs": true, "rpc_pipefs": true, "nsfs": true,
	"efivarfs": true, "rootfs": true, "overlay": true, "squashfs": true, "binfmt_misc": true,
}

// /proc/mounts 里空格/制表符/反斜杠以八进制转义
func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func sameDev(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	if ea == nil && eb == nil {
		return ra == rb
	}
	return filepath.Base(a) == filepath.Base(b)
}

// payloadAndRootDev 从挂载表解析出承载 payload_dir 与 / 的设备。
func (p *Probe) payloadAndRootDev(mounts []mountEntry, payloadRoot string) (payloadDev, rootDev string) {
	best := -1
	for _, m := range mounts {
		if m.Target == "/" {
			rootDev = m.Dev
		}
		if payloadRoot == "" {
			continue
		}
		if m.Target == payloadRoot || strings.HasPrefix(payloadRoot, m.Target+"/") {
			if len(m.Target) > best {
				best = len(m.Target)
				payloadDev = m.Dev
			}
		}
	}
	return payloadDev, rootDev
}

// ---------- 设备枚举 ----------

// List 枚举所有可操作的块设备（整盘 + 分区），并标注每个设备是否“受保护”。
// payloadRoot 传空则只按“已挂载”判定；传 ctx.PayloadRoot() 可额外标注数据分区。
func (p *Probe) List(payloadRoot string) ([]Disk, error) {
	mounts := p.readMounts()
	mountedDev := map[string]string{}
	for _, m := range mounts {
		mountedDev[m.Dev] = m.Target
	}

	sys := p.sysBlock()
	entries, err := os.ReadDir(sys)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", sys, err)
	}

	partNums := map[string][]int{}
	var diskNames []string
	for _, e := range entries {
		name := e.Name()
		if skipDevice(name) {
			continue
		}
		if fileExists(filepath.Join(sys, name, "partition")) {
			num, _ := strconv.Atoi(readTrim(filepath.Join(sys, name, "partition")))
			if d := p.parentDisk(name); d != "" {
				partNums[d] = append(partNums[d], num)
			}
			continue
		}
		diskNames = append(diskNames, name)
	}
	sort.Strings(diskNames)

	var disks []Disk
	for _, name := range diskNames {
		d := Disk{
			Name:       name,
			Path:       p.pubDev(name),
			SizeBytes:  p.sizeBytes(name),
			Removable:  readTrim(filepath.Join(sys, name, "removable")) == "1",
			ReadOnly:   readTrim(filepath.Join(sys, name, "ro")) == "1",
			Model:      readTrim(filepath.Join(sys, name, "device", "model")),
			Transport:  p.transport(name),
			Serial:     p.diskSerial(name),
			WWID:       p.diskWWID(name),
			Partitions: []Partition{},
		}
		d.SizeHuman = HumanSize(d.SizeBytes)
		d.Protected, d.ProtectWhy = p.protectionOf(d.Path, payloadRoot)

		nums := partNums[name]
		sort.Ints(nums)
		for _, n := range nums {
			pname := p.partName(name, n)
			part := Partition{
				Name:      pname,
				Path:      p.pubDev(pname),
				Number:    n,
				SizeBytes: p.sizeBytes(pname),
			}
			part.SizeHuman = HumanSize(part.SizeBytes)
			part.Protected, part.ProtectWhy = p.protectionOf(part.Path, payloadRoot)
			p.fillFS(&part.Filesystem, &part.Label, &part.UUID, &part.MountPoint, pname, mountedDev)
			d.Partitions = append(d.Partitions, part)
		}

		if len(nums) == 0 {
			// 无分区表的裸盘（如 Ventoy 数据盘常是整盘一个 exFAT/ext4）
			p.fillFS(&d.Filesystem, &d.Label, &d.UUID, &d.MountPoint, name, mountedDev)
		}
		if !d.Protected && (len(nums) > 0 || d.Filesystem != "") {
			d.PartTable = p.partTable(name)
		}
		disks = append(disks, d)
	}
	return disks, nil
}

// FillFS 供调用方复用同一套文件系统探测逻辑（如 format 后回读 UUID）。
func (p *Probe) FillFS(fs, label, uuid, mount *string, name string) {
	mounted := map[string]string{}
	for _, m := range p.readMounts() {
		mounted[m.Dev] = m.Target
	}
	p.fillFS(fs, label, uuid, mount, name, mounted)
}

func (p *Probe) fillFS(fs, label, uuid, mount *string, name string, mountedDev map[string]string) {
	dev := p.devPath(name)
	for d, target := range mountedDev {
		if sameDev(d, dev) {
			*mount = target
			break
		}
	}
	if out, err := p.runner()("blkid", dev); err == nil {
		if v := kvValue(out, "TYPE"); v != "" {
			*fs = v
		}
		if v := kvValue(out, "LABEL"); v != "" {
			*label = v
		}
		if v := kvValue(out, "UUID"); v != "" {
			*uuid = v
		}
	}
}

// partName 兼容 sd/vd（vda1）与 nvme/mmcblk（nvme0n1p1）两种命名。
func (p *Probe) partName(disk string, n int) string {
	if strings.HasPrefix(disk, "nvme") || strings.HasPrefix(disk, "mmcblk") {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// parentDisk 通过 realpath 反查分区所属整盘（sd/vd/nvme/mmcblk 都适用）。
func (p *Probe) parentDisk(part string) string {
	rp, err := filepath.EvalSymlinks(filepath.Join(p.sysBlock(), part))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(rp))
}

func (p *Probe) parentDiskOfPath(dev string) string {
	if dev == "" || !strings.HasPrefix(dev, "/") {
		return ""
	}
	return p.parentDisk(filepath.Base(dev))
}

func (p *Probe) sizeBytes(name string) int64 {
	s := readTrim(filepath.Join(p.sysBlock(), name, "size"))
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * 512
}

func (p *Probe) transport(name string) string {
	switch {
	case strings.HasPrefix(name, "nvme"):
		return "nvme"
	case strings.HasPrefix(name, "mmcblk"):
		return "mmc"
	case strings.HasPrefix(name, "vd"):
		return "virtio"
	case strings.HasPrefix(name, "sr"):
		return "cdrom"
	}
	if link, err := filepath.EvalSymlinks(filepath.Join(p.sysBlock(), name, "device")); err == nil {
		if strings.Contains(link, "/usb") {
			return "usb"
		}
		if strings.Contains(link, "virtio") {
			return "virtio"
		}
	}
	if strings.HasPrefix(name, "sd") {
		return "sata"
	}
	return ""
}

// diskSerial 返回整盘的序列号：virtio-blk 在 <dev>/serial；SCSI/SATA 在 <dev>/device/serial
// （新内核已移除该属性，故常为空，此时退化为 diskWWID）。取不到返回空串。
func (p *Probe) diskSerial(name string) string {
	for _, rel := range []string{"serial", filepath.Join("device", "serial")} {
		if v := readTrim(filepath.Join(p.sysBlock(), name, rel)); v != "" {
			return v
		}
	}
	return ""
}

// diskWWID 返回 SCSI/SATA 的 WWN 标识（如 naa.50014ee2bb6371dc）；USB/virtio 常为空。
func (p *Probe) diskWWID(name string) string {
	return readTrim(filepath.Join(p.sysBlock(), name, "device", "wwid"))
}

// DiskIdent 返回整盘可用于 autoinstall 模板匹配的稳定标识（serial 优先，其次 wwid），
// 以及用了哪一类（"serial"/"wwid"）；两者都没有时返回空（调用方应退回设备路径并告警）。
func (p *Probe) DiskIdent(dev string) (value, kind string) {
	name := filepath.Base(dev)
	if s := p.diskSerial(name); s != "" {
		return s, "serial"
	}
	if w := p.diskWWID(name); w != "" {
		return w, "wwid"
	}
	return "", ""
}

// partTable 用 `parted print` 读出分区表类型（gpt/msdos）；无标签时返回空。
func (p *Probe) partTable(name string) string {
	out, _ := p.runner()("parted", "-s", p.devPath(name), "print")
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "Partition Table:"); i >= 0 {
			v := strings.TrimSpace(line[i+len("Partition Table:"):])
			if strings.EqualFold(v, "unknown") || v == "" {
				return ""
			}
			return v
		}
	}
	return ""
}

// ---------- 小工具 ----------

func skipDevice(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "dm-", "sr", "fd", "md"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// kvValue 从 blkid 输出里取 KEY="VALUE"。busybox 与 util-linux 的 blkid 都符合该形式。
func kvValue(out, key string) string {
	needle := key + "=\""
	i := strings.Index(out, needle)
	if i < 0 {
		return ""
	}
	rest := out[i+len(needle):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// HumanSize 把字节数格式化为 IEC 单位（KiB/MiB/GiB...）。
func HumanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
