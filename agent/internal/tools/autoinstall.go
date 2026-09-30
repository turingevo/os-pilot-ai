package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"os-pilot-ai/internal/disk"
)

// gen_autoinstall：确定性生成 Ubuntu autoinstall 应答脚本（storage 段由代码拼装）。
// 选盘用磁盘序列号（serial），挂载用卷标（label）；不再让模型手写、也不硬编码 /dev/vdX。
type genAutoinstallArgs struct {
	Image        string                 `json:"image"`
	Disk         string                 `json:"disk"`
	Partitions   []disk.AutoinstallPart `json:"partitions"`
	Output       string                 `json:"output"`
	Username     string                 `json:"username"`
	Hostname     string                 `json:"hostname"`
	PasswordHash string                 `json:"password_hash"`
	Locale       string                 `json:"locale"`
	Timezone     string                 `json:"timezone"`
}

func handleGenAutoinstall(ctx *Context, raw json.RawMessage) (string, error) {
	var a genAutoinstallArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝生成自动安装模板）", nil
	}
	if strings.TrimSpace(a.Disk) == "" {
		return "", fmt.Errorf("disk 不能为空：请传 list_disks 里的整盘设备（如 /dev/sdb）")
	}
	if strings.TrimSpace(a.Image) == "" {
		return "", fmt.Errorf("image 不能为空：请传数据分区内的镜像路径（如 /ISO/ubuntu-24.04.iso）")
	}

	probe := probeOf(ctx)
	// 复用 partition 的校验/守卫：确认是整盘、存在、且不承载运行环境（模板会 wipe 这块盘）。
	specs := make([]disk.PartSpec, 0, len(a.Partitions))
	for _, p := range a.Partitions {
		specs = append(specs, disk.PartSpec{Size: p.Size, FS: p.FS, Name: p.Label})
	}
	if _, err := probe.PartitionPlan(a.Disk, "gpt", specs, ctx.PayloadRoot()); err != nil {
		return "", err
	}

	// 镜像必须在数据分区内真实存在（Ventoy 的数据分区路径就是 payload_dir）。
	imgRel := strings.TrimPrefix(a.Image, "/")
	imgFull, err := resolvePayloadPath(ctx, imgRel)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(imgFull); statErr != nil {
		return "", fmt.Errorf("镜像不存在: %s（解析为 %s）", a.Image, imgFull)
	}

	ident, kind := probe.DiskIdent(a.Disk)

	outPath := strings.TrimSpace(a.Output)
	if outPath == "" {
		base := strings.TrimSuffix(filepath.Base(a.Image), filepath.Ext(a.Image))
		outPath = "autoinstall-" + base + ".yaml"
	}
	full, err := resolvePayloadPath(ctx, strings.TrimPrefix(outPath, "/"))
	if err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(ctx.PayloadRoot(), full)

	text, err := disk.AutoinstallTemplate(disk.AutoinstallOptions{
		Image:        a.Image,
		Disk:         a.Disk,
		Ident:        ident,
		IdentKind:    kind,
		Parts:        a.Partitions,
		Username:     a.Username,
		Hostname:     a.Hostname,
		PasswordHash: a.PasswordHash,
		Locale:       a.Locale,
		Timezone:     a.Timezone,
	})
	if err != nil {
		return "", err
	}

	oldText := ""
	if b, err := os.ReadFile(full); err == nil {
		oldText = string(b)
	}
	if oldText == text {
		return "模板内容无变化，未写入。", nil
	}
	if ctx.Mode != "direct" {
		ok, err := ctx.IO.Confirm(fmt.Sprintf("生成自动安装模板 %s（%d 字节）", rel, len(text)), UnifiedDiff(rel, oldText, text))
		if err != nil {
			return "", err
		}
		if !ok {
			return "用户拒绝了这次写入。", nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("写入失败: %w", err)
	}

	res := map[string]any{
		"ok":       true,
		"action":   "gen_autoinstall",
		"target":   full,
		"path":     rel,
		"image":    a.Image,
		"disk":     a.Disk,
		"template": text,
	}
	if kind == "serial" && ident != "" {
		res["disk_ident"] = ident
		res["disk_ident_kind"] = "serial"
		res["note"] = "已用磁盘序列号选盘（与 /dev/vdX、/dev/sdX 无关）；分区挂载用卷标。"
	} else {
		res["disk_ident_kind"] = "path"
		res["warning"] = "该盘读不到序列号（serial），模板退回按设备路径 " + a.Disk + " 选盘；换接口/换机器可能错位，请核对。"
	}
	return jsonResult(res)
}
