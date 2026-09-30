package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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
		if count <= 200 {
			files = append(files, map[string]any{"path": "/" + filepath.ToSlash(rel), "size": fi.Size()})
		}
		return nil
	})
	payload["files"] = files
	payload["file_count"] = count
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

	out, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	if len(out) > 131072 {
		out = append(out[:131072], []byte("...(截断)")...)
	}
	return string(out), nil
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
	return out
}
