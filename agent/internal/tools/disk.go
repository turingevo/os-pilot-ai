package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"os-pilot-ai/internal/disk"
)

func probeOf(ctx *Context) *disk.Probe {
	if ctx.Disks != nil {
		return ctx.Disks
	}
	return disk.DefaultProbe()
}

func jsonResult(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// confirmDestructive 处理破坏性操作（改分区表/格式化）的确认闸门：
// orchestrate（默认）下要求用户逐字输入目标设备名；direct 模式视为用户已授权，直接放行。
// 受保护设备与参数白名单是硬守卫，在调用本函数之前已生效，任何模式都绕不过。
func (ctx *Context) confirmDestructive(target string, steps []string) (bool, error) {
	if ctx.Mode == "direct" {
		return true, nil
	}
	detail := "将要执行：\n  " + strings.Join(steps, "\n  ")
	return ctx.IO.TypedConfirm(fmt.Sprintf("即将对 %s 执行破坏性操作（数据将丢失）", target), detail, filepath.Base(target))
}

func (ctx *Context) confirmBackup(dst string, steps []string, del bool) (bool, error) {
	if ctx.Mode == "direct" {
		return true, nil
	}
	detail := "将要执行：\n  " + strings.Join(steps, "\n  ")
	if del {
		return ctx.IO.TypedConfirm("即将执行 rsync（含 --delete：会删除目标端多余文件）", detail, filepath.Base(dst))
	}
	return ctx.IO.Confirm("即将备份到 "+dst, detail)
}

// ---------- list_disks ----------

func handleListDisks(ctx *Context, raw json.RawMessage) (string, error) {
	disks, err := probeOf(ctx).List(ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	return jsonResult(map[string]any{
		"payload_dir": ctx.PayloadRoot(),
		"disks":       disks,
		"note":        "protected=true 的设备承载运行中的系统/数据分区，或已被挂载，禁止改动；写入前必须让用户明确选定目标盘。",
	})
}

// ---------- partition ----------

type partitionToolArgs struct {
	Disk       string          `json:"disk"`
	Table      string          `json:"table"`
	Partitions []disk.PartSpec `json:"partitions"`
}

func handlePartition(ctx *Context, raw json.RawMessage) (string, error) {
	var a partitionToolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝改分区表）", nil
	}
	probe := probeOf(ctx)
	steps, err := probe.PartitionPlan(a.Disk, a.Table, a.Partitions, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	ok, err := ctx.confirmDestructive(a.Disk, steps)
	if err != nil {
		return "", err
	}
	if !ok {
		return "用户取消了这次分区操作。", nil
	}
	res, err := probe.Partition(a.Disk, a.Table, a.Partitions, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	return jsonResult(res)
}

// ---------- format ----------

type formatToolArgs struct {
	Device string `json:"device"`
	FSType string `json:"fstype"`
	Label  string `json:"label"`
}

func handleFormat(ctx *Context, raw json.RawMessage) (string, error) {
	var a formatToolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝格式化）", nil
	}
	probe := probeOf(ctx)
	steps, err := probe.FormatPlan(a.Device, a.FSType, a.Label, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	ok, err := ctx.confirmDestructive(a.Device, steps)
	if err != nil {
		return "", err
	}
	if !ok {
		return "用户取消了这次格式化。", nil
	}
	res, err := probe.Format(a.Device, a.FSType, a.Label, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	return jsonResult(res)
}

// ---------- backup ----------

type backupToolArgs struct {
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Delete bool   `json:"delete"`
	DryRun bool   `json:"dry_run"`
}

func handleBackup(ctx *Context, raw json.RawMessage) (string, error) {
	var a backupToolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝执行备份写入）", nil
	}
	if strings.TrimSpace(a.Src) == "" {
		a.Src = ctx.PayloadRoot()
	}
	probe := probeOf(ctx)
	steps, err := probe.BackupPlan(a.Src, a.Dst, a.Delete, a.DryRun, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	ok, err := ctx.confirmBackup(a.Dst, steps, a.Delete)
	if err != nil {
		return "", err
	}
	if !ok {
		return "用户取消了这次备份。", nil
	}
	res, err := probe.Backup(a.Src, a.Dst, a.Delete, a.DryRun, ctx.PayloadRoot())
	if err != nil {
		return "", err
	}
	return jsonResult(res)
}
