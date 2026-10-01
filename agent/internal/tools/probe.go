package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// 探测结果要整段塞进模型上下文，必须有 token 预算：实测一份 210 个文件的 U 盘清单
// （含上百个 DLL 路径）单独就占 7085 token，叠加系统提示与 11 个工具定义后达到 10615
// token，直接把 8192 上下文的本地模型撑成 HTTP 400 (exceed_context_size_error)。
const (
	// payloadFileListMax 是 payload.files 的条目上限。Walk 按字典序，U 盘上真正要紧的
	// 根目录镜像与 /ventoy 配置都排在前面，超限截掉的是成套软件目录里的碎片文件。
	payloadFileListMax = 60
	// payloadImageMax 是 payload.images 的条目上限。
	payloadImageMax = 30
	// rawListMax 限制 blkid / 挂载表条数：开发机这类环境有上百个 loop/snap 挂载，
	// 实测一份挂载表单独就占 2137 token，比 U 盘清单还大。
	blkidListMax  = 40
	mountsListMax = 60
	// probeOutputMax 是整个探测结果的字节上限。实测这份内容 ~2.3 字节/token，
	// 12288 字节约 5.3k token，叠加系统提示与 11 个工具定义（实测 3298 token）
	// 仍在最小的 8192 上下文窗口内。
	probeOutputMax = 12288
)

// imageExts 是Ventoy/安装器可作为安装源的镜像后缀（大小写不敏感）。
var imageExts = map[string]bool{
	".iso": true, ".img": true, ".wim": true, ".vhd": true, ".vhdx": true,
	".raw": true, ".qcow2": true, ".vmdk": true,
}

func handleSystemProbe(ctx *Context, raw json.RawMessage) (string, error) {
	info := map[string]any{}

	if b, err := os.ReadFile("/proc/version"); err == nil {
		info["kernel"] = strings.TrimSpace(string(b))
	}

	cpu := map[string]any{}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		count := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "processor") {
				count++
			}
			if strings.HasPrefix(line, "model name") && cpu["model"] == nil {
				if idx := strings.Index(line, ":"); idx >= 0 {
					cpu["model"] = strings.TrimSpace(line[idx+1:])
				}
			}
		}
		cpu["cores"] = count
	}
	info["cpu"] = cpu

	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int
				fmt.Sscanf(line, "MemTotal: %d kB", &kb)
				info["mem_total_mb"] = kb / 1024
				break
			}
		}
	}

	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		var ifs []string
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, ":") {
				continue
			}
			name := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
			if name != "" && name != "lo" {
				ifs = append(ifs, name)
			}
		}
		info["net_ifaces"] = ifs
	}
	if b, err := os.ReadFile("/proc/net/route"); err == nil {
		def := ""
		for i, line := range strings.Split(string(b), "\n") {
			if i == 0 || strings.TrimSpace(line) == "" {
				continue
			}
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "00000000" {
				def = f[0]
			}
		}
		info["default_route_iface"] = def
	}

	if out, _ := runCapture("blkid"); strings.TrimSpace(out) != "" {
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if strings.TrimSpace(l) != "" {
				lines = append(lines, strings.TrimSpace(l))
			}
		}
		if len(lines) > blkidListMax {
			lines = append(lines[:blkidListMax:blkidListMax],
				fmt.Sprintf("...(blkid 共 %d 条，只列前 %d 条；完整信息用 run_command 执行 blkid)", len(lines), blkidListMax))
		}
		info["blkid"] = lines
	}
	if out, _ := runCapture("fdisk", "-l"); strings.TrimSpace(out) != "" {
		s := out
		if len(s) > 8192 {
			s = s[:8192] + "\n...(截断)"
		}
		info["fdisk"] = s
	}
	if b, err := os.ReadFile("/proc/partitions"); err == nil {
		info["partitions"] = strings.TrimSpace(string(b))
	}

	if b, err := os.ReadFile("/proc/mounts"); err == nil {
		info["mounts"] = filterMounts(string(b))
	}

	payload := map[string]any{"root": ctx.PayloadRoot()}
	var files []map[string]any
	var images []map[string]any
	count := 0
	_ = filepath.Walk(ctx.PayloadRoot(), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if p == ctx.PayloadRoot() {
			return nil
		}
		rel, err := filepath.Rel(ctx.PayloadRoot(), p)
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if strings.Count(rel, string(filepath.Separator)) >= 3 {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		slash := "/" + filepath.ToSlash(rel)
		if count <= payloadFileListMax {
			files = append(files, map[string]any{"path": slash, "size": fi.Size()})
		}
		if imageExts[strings.ToLower(filepath.Ext(slash))] {
			images = append(images, map[string]any{"path": slash, "size": fi.Size()})
		}
		return nil
	})
	sort.Slice(images, func(i, j int) bool {
		return images[i]["size"].(int64) > images[j]["size"].(int64)
	})
	if len(images) > payloadImageMax {
		images = images[:payloadImageMax]
	}
	payload["files"] = files
	payload["file_count"] = count
	payload["files_shown"] = len(files)
	if count > payloadFileListMax {
		payload["files_truncated"] = true
		payload["files_hint"] = fmt.Sprintf(
			"清单只列前 %d 项（共 %d 个文件）；需要看完整目录用 run_command（如 ls -R /ISO）或 fs_read",
			payloadFileListMax, count)
	}
	if len(images) > 0 {
		payload["images"] = images
	}
	if b, err := os.ReadFile(filepath.Join(ctx.PayloadRoot(), "ventoy", "ventoy.json")); err == nil {
		s := string(b)
		if len(s) > 4096 {
			s = s[:4096] + "\n...(截断)"
		}
		payload["ventoy_json"] = s
	}
	if _, err := os.Stat(filepath.Join(ctx.PayloadRoot(), "ventoy", "ai", "vmlinuz")); err == nil {
		payload["ai_env_on_disk"] = true
	}
	info["payload"] = payload

	out, err := marshalWithinBudget(info)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// marshalWithinBudget 超预算时按"先丢最不要紧的"顺序逐级降级，且每级都保证输出仍是
// 合法 JSON：从 JSON 中间硬切一刀会让模型看到无法解析的片段，比少列几项更糟。
// 让位顺序：挂载表 → blkid → fdisk → 文件清单 → ventoy.json 原文。
// 前三项是固定开销且模型随时能用 run_command 现场取回；文件清单是这台盘"有什么"的核心答案。
func marshalWithinBudget(info map[string]any) ([]byte, error) {
	out, err := json.Marshal(info)
	if err != nil || len(out) <= probeOutputMax {
		return out, err
	}
	info["mounts"] = cutList(info["mounts"], 16, "挂载")
	if out, err = json.Marshal(info); err != nil || len(out) <= probeOutputMax {
		return out, err
	}
	info["blkid"] = cutList(info["blkid"], 16, "blkid")
	if out, err = json.Marshal(info); err != nil || len(out) <= probeOutputMax {
		return out, err
	}
	if s, ok := info["fdisk"].(string); ok && len(s) > 2048 {
		info["fdisk"] = s[:2048] + "\n...(fdisk 输出截断；完整信息用 run_command 执行 fdisk -l)"
	} else if ok {
		delete(info, "fdisk")
	}
	if out, err = json.Marshal(info); err != nil || len(out) <= probeOutputMax {
		return out, err
	}
	if payload, ok := info["payload"].(map[string]any); ok {
		delete(payload, "files")
		payload["files_dropped"] = fmt.Sprintf(
			"探测结果超出 %d 字节预算，已省略文件清单（共 %v 个文件）；用 run_command（如 ls -R /ISO）或 fs_read 查看",
			probeOutputMax, payload["file_count"])
		if out, err = json.Marshal(info); err != nil || len(out) <= probeOutputMax {
			return out, err
		}
		delete(payload, "ventoy_json")
		payload["ventoy_json_dropped"] = "ventoy.json 原文已省略，用 fs_read 读取 /ventoy/ventoy.json"
		if out, err = json.Marshal(info); err != nil || len(out) <= probeOutputMax {
			return out, err
		}
	}
	return append(out[:probeOutputMax], []byte("...(截断)")...), nil
}

// cutList 把字符串列表裁到前 n 条，并追加一条说明告诉模型怎么取完整信息。
func cutList(v any, n int, label string) any {
	l, ok := v.([]string)
	if !ok || len(l) <= n {
		return v
	}
	kept := append([]string{}, l[:n]...)
	return append(kept, fmt.Sprintf("...(%s 共 %d 条，只列前 %d 条；完整信息用 run_command 获取)", label, len(l), n))
}

func runCapture(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Env = []string{"PATH=/bin:/sbin:/usr/bin:/usr/sbin"}
	out, err := c.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

var pseudoFs = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"securityfs": true, "cgroup": true, "cgroup2": true, "pstore": true, "bpf": true,
	"tracefs": true, "debugfs": true, "configfs": true, "fusectl": true, "mqueue": true,
	"hugetlbfs": true, "ramfs": true, "autofs": true, "rpc_pipefs": true, "nsfs": true,
	"efivarfs": true,
}

func filterMounts(data string) []string {
	var out []string
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || pseudoFs[f[2]] {
			continue
		}
		out = append(out, fmt.Sprintf("%s on %s type %s (%s)", f[0], f[1], f[2], f[3]))
	}
	if len(out) > mountsListMax {
		out = append(out[:mountsListMax:mountsListMax],
			fmt.Sprintf("...(挂载共 %d 条，只列前 %d 条；完整信息用 run_command 执行 cat /proc/mounts)", len(out), mountsListMax))
	}
	return out
}
