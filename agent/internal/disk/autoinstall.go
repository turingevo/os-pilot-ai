package disk

import (
	"fmt"
	"strings"
)

// 本文件负责“确定性生成 Ubuntu autoinstall 应答脚本”。不交给模型手写的原因：
// 模型会硬编码 /dev/vdX（换接口/换机器就错），并写出非 schema 的字段
// （type: fs / device: / fstab_id）。curtin 的实际 schema 是：
//   type: disk      → 用 serial 或 path 选盘
//   type: partition → device: <disk 的 id>
//   type: format    → volume: <partition 的 id>          （不是 device）
//   type: mount     → device: <format 的 id>, path: <挂载点>
// 所以这里用固定结构拼装，只把用户给的分区方案填进去；盘名/标识、字段名都由代码保证。

// templateFS 是模板里允许出现的文件系统类型（由安装器解释，不经过本包的工具链）。
// 刻意与 fsSpecs 解耦、只保留 Linux 装机能用的类型：装机根分区不能是 exFAT/NTFS，
// 若跟着"能格式化的清单"放宽，模型会以为能把系统装进 exFAT 分区。
var templateFS = map[string]bool{"ext4": true, "ext3": true, "ext2": true, "vfat": true}

// AutoinstallPart 描述目标盘上的一个分区。
type AutoinstallPart struct {
	Size  string `json:"size"`           // 512M / 8G / rest
	Label string `json:"label"`          // 卷标：既是可读名，也是 fstab 的稳定标识
	Mount string `json:"mount"`          // 挂载点：/ 或 /home 或 /boot/efi
	FS    string `json:"fs,omitempty"`   // 留空则按挂载点推断（/boot/efi → vfat，其余 ext4）
	Boot  bool   `json:"boot,omitempty"` // 是否加 boot 标志（EFI 分区需要）
}

// AutoinstallOptions 是生成模板所需的全部输入。
type AutoinstallOptions struct {
	Image        string // Ventoy 数据分区内的镜像路径，如 /ISO/ubuntu-24.04.iso
	Disk         string // 对外设备路径，仅用于提示与回退
	Ident        string // 稳定标识值（serial）
	IdentKind    string // "serial"（用 serial 选盘）/ 其它（退回 path）
	Parts        []AutoinstallPart
	Username     string
	Hostname     string
	PasswordHash string
	Locale       string
	Timezone     string
}

// AutoinstallTemplate 生成 cloud-config 形式的 autoinstall 应答脚本。
// 选盘：有 serial 就用 serial（与设备名无关），否则退回 path 并在返回值里说明。
func AutoinstallTemplate(o AutoinstallOptions) (string, error) {
	if !strings.HasPrefix(o.Image, "/") || strings.ContainsAny(o.Image, "\n\r\"") {
		return "", fmt.Errorf("image 必须是数据分区内的绝对路径（如 /ISO/ubuntu-24.04.iso）")
	}
	if len(o.Parts) == 0 {
		return "", fmt.Errorf("至少需要一个分区")
	}
	if strings.ContainsAny(o.Ident, "\n\r\"") {
		return "", fmt.Errorf("磁盘标识含非法字符: %q", o.Ident)
	}

	seen := map[string]bool{}
	hasRoot := false
	for i, p := range o.Parts {
		if _, rest, err := parseSize(p.Size); err != nil {
			return "", fmt.Errorf("第 %d 个分区: %w", i+1, err)
		} else if rest && i != len(o.Parts)-1 {
			return "", fmt.Errorf("第 %d 个分区用了 rest/100%%，只能放在最后一项", i+1)
		}
		if p.Label == "" {
			return "", fmt.Errorf("第 %d 个分区: 卷标必须给（它是 fstab 的稳定标识）", i+1)
		}
		if err := validateLabel(p.Label); err != nil {
			return "", fmt.Errorf("第 %d 个分区: %w", i+1, err)
		}
		if !strings.HasPrefix(p.Mount, "/") || strings.ContainsAny(p.Mount, "\n\r \t") {
			return "", fmt.Errorf("第 %d 个分区: 挂载点必须是绝对路径（如 / 或 /home），实际 %q", i+1, p.Mount)
		}
		if seen[p.Mount] {
			return "", fmt.Errorf("挂载点重复: %s", p.Mount)
		}
		seen[p.Mount] = true
		if p.Mount == "/" {
			hasRoot = true
		}
		if fs := partFS(p); !templateFS[fs] {
			return "", fmt.Errorf("第 %d 个分区: 不支持的文件系统 %q（可用 ext2/ext3/ext4/vfat）", i+1, fs)
		}
	}
	if !hasRoot {
		return "", fmt.Errorf("必须有一个分区挂载到 /")
	}

	username := o.Username
	if username == "" {
		username = "ubuntu"
	}
	hostname := o.Hostname
	if hostname == "" {
		hostname = "ubuntu"
	}
	locale := o.Locale
	if locale == "" {
		locale = "zh_CN.UTF-8"
	}
	tz := o.Timezone
	if tz == "" {
		tz = "Asia/Shanghai"
	}

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("# 由 AI 装机助手生成（gen_autoinstall）——请勿手改设备名：选盘用磁盘标识，挂载用卷标。\n")
	b.WriteString("autoinstall:\n")
	b.WriteString("  version: 1\n")
	fmt.Fprintf(&b, "  locale: %s\n", locale)
	b.WriteString("  keyboard:\n    layout: us\n")
	fmt.Fprintf(&b, "  timezone: %s\n", tz)
	b.WriteString("  identity:\n")
	fmt.Fprintf(&b, "    hostname: %s\n", hostname)
	fmt.Fprintf(&b, "    username: %s\n", username)
	if o.PasswordHash != "" {
		fmt.Fprintf(&b, "    password: \"%s\"\n", o.PasswordHash)
	} else {
		b.WriteString("    # 未提供 password_hash：账号将被锁定，安装后需自行设置密码\n")
	}
	b.WriteString("  storage:\n    version: 1\n    config:\n")

	// 选盘：serial 是稳定标识（与 /dev/vdX、/dev/sdX 无关）；拿不到就退回设备路径。
	if o.IdentKind == "serial" && o.Ident != "" {
		fmt.Fprintf(&b, "      - {id: disk0, type: disk, ptable: gpt, serial: \"%s\", wipe: superblock, grub_device: true}\n", o.Ident)
	} else {
		fmt.Fprintf(&b, "      - {id: disk0, type: disk, ptable: gpt, path: %s, wipe: superblock, grub_device: true}\n", o.Disk)
	}

	for i, p := range o.Parts {
		pid := fmt.Sprintf("part%d", i+1)
		fs := partFS(p)
		flags := ""
		if p.Boot {
			flags = ", flag: boot"
		}
		// 分区大小：rest 用 -1（curtin 语义：占满剩余空间）
		size := p.Size
		if s := strings.TrimSpace(strings.ToLower(size)); s == "rest" || s == "100%" || s == "-" {
			size = "-1"
		}
		fmt.Fprintf(&b, "      - {id: %s, type: partition, device: disk0, number: %d, size: %s%s}\n", pid, i+1, size, flags)
		fmt.Fprintf(&b, "      - {id: %s-fmt, type: format, volume: %s, fstype: %s, label: \"%s\"}\n", pid, pid, fs, p.Label)
		fmt.Fprintf(&b, "      - {id: %s-mnt, type: mount, device: %s-fmt, path: %s}\n", pid, pid, p.Mount)
	}
	return b.String(), nil
}

func partFS(p AutoinstallPart) string {
	if p.FS != "" {
		return p.FS
	}
	if p.Boot || p.Mount == "/boot/efi" || p.Mount == "/boot" {
		return "vfat"
	}
	return "ext4"
}
