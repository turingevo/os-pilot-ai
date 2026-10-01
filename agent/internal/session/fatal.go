package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FatalSection 是诊断文件里的一段多行证据（例如 panic 调用栈），用代码块原样保留。
type FatalSection struct{ Title, Body string }

// DumpFatal 在会话文件还没建立起来时就退出，盘上会完全没有现场证据——真机上
// "屏幕闪一下就没电"正是这种路径。这里补一份最小留档，命名与 session-*.md 同目录，
// 让人只需找最新的文件。返回写入的路径（失败时为空，调用方不该因为记不上而改变退出码）。
//
// info 的内容会被原样写入，所以只放非敏感字段：诊断文件长期留在用户 U 盘上，
// api_key 一类的值绝不能进来。
func DumpFatal(dir string, code int, info map[string]string, extra ...FatalSection) string {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "ventoy-ai-logs")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ""
	}
	now := time.Now()
	p := filepath.Join(dir, "fatal-"+now.Format("20060102-150405")+".md")
	f, err := os.Create(p)
	if err != nil {
		return ""
	}
	defer f.Close()

	fmt.Fprintf(f, "# os-pilot-ai 启动失败\n\n- time: %s\n- rc: %d\n\n",
		now.Format("2006-01-02 15:04:05"), code)
	keys := make([]string, 0, len(info))
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	writeKV := func(k, v string) {
		if isSecretKey(k) {
			v = "(已屏蔽：诊断文件长期留在 U 盘上)"
		}
		if v == "" {
			v = "(空)"
		}
		fmt.Fprintf(f, "- %s: %s\n", k, strings.ReplaceAll(v, "\n", " / "))
	}
	if len(keys) > 0 {
		fmt.Fprint(f, "#### 上下文\n\n")
		if v, ok := info["reason"]; ok {
			writeKV("reason", v)
		}
		for _, k := range keys {
			if k == "reason" {
				continue
			}
			writeKV(k, info[k])
		}
	}

	// 环境自查：这几项覆盖了真机失败的大部分成因（内存不够、数据分区没挂上、模型没起来）。
	fmt.Fprint(f, "\n#### 环境自查\n\n")
	writeKV("meminfo", memTotal())
	writeKV("payload_mounted", payloadMounted())

	for _, s := range extra {
		if s.Body == "" {
			continue
		}
		fmt.Fprintf(f, "\n#### %s\n\n```\n%s\n```\n", s.Title, strings.TrimRight(s.Body, "\n"))
	}

	if tail := bootLogTail(info["boot_log"]); tail != "" {
		fmt.Fprintf(f, "\n#### 启动日志末尾（%s）\n\n```\n%s\n```\n", info["boot_log"], tail)
	}
	return p
}

// isSecretKey 认字段名而不是值：调用方用的是白名单，这里只是兜底，
// 防止以后有人图省事把整个配置对象塞进诊断上下文。
func isSecretKey(k string) bool {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case "api_key", "apikey", "authorization", "key", "token", "secret", "password", "passwd":
		return true
	}
	return false
}

func memTotal() string {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// payloadMounted 看 /proc/mounts 里数据分区到底挂上了没有：挂载失败时 agent 后面
// 所有的"找不到模型/找不到配置"都是同一个根因，先把它挑明。
func payloadMounted() string {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == "/iso" {
			return f[0] + " → /iso (" + f[2] + ")"
		}
	}
	return "/iso 未挂载"
}

// bootLogTail 取 init 那份启动日志的末尾若干行，直接嵌进诊断文件：
// 真机上没人愿意对着两个文件名和时钟做交叉比对。
func bootLogTail(rel string) string {
	if rel == "" {
		return ""
	}
	p := rel
	if !strings.HasPrefix(p, "/") {
		p = filepath.Join("/iso", rel)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	const keep = 40
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return strings.Join(lines, "\n")
}
