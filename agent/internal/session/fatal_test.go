package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpFatalWritesEvidenceWithSections(t *testing.T) {
	dir := t.TempDir()
	bootLog := filepath.Join(dir, "boot-x.log")
	if err := os.WriteFile(bootLog, []byte(strings.Repeat("行\n", 60)), 0644); err != nil {
		t.Fatal(err)
	}
	info := map[string]string{
		"reason":   "配置错误: base_url 未配置",
		"boot_log": bootLog,
	}
	p := DumpFatal(dir, 2, info, FatalSection{Title: "调用栈", Body: "goroutine 1 [running]:\nmain.run()\n"})
	if p == "" {
		t.Fatal("未写出诊断文件")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"rc: 2", "配置错误: base_url 未配置", "#### 调用栈", "main.run()", "#### 启动日志末尾"} {
		if !strings.Contains(got, want) {
			t.Errorf("诊断文件缺少 %q", want)
		}
	}
	// 启动日志只嵌末尾若干行，不能整份抄进来
	if strings.Count(got, "行") > 45 {
		t.Errorf("启动日志未按末尾截断（嵌了 %d 行）", strings.Count(got, "行"))
	}
	if !strings.HasPrefix(filepath.Base(p), "fatal-") || !strings.HasSuffix(p, ".md") {
		t.Errorf("命名应为 fatal-<时间>.md，得到 %s", filepath.Base(p))
	}
}

func TestDumpFatalNeverRecordsSecrets(t *testing.T) {
	// 诊断文件长期留在用户 U 盘上：调用方传入的 key/token 一律不得出现。
	dir := t.TempDir()
	p := DumpFatal(dir, 2, map[string]string{"reason": "x", "api_key": "sk-SECRET-VALUE"})
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "sk-SECRET-VALUE") {
		t.Error("诊断文件写入了调用方传入的密钥值")
	}
}

func TestLoggerEndPairsWithStart(t *testing.T) {
	l, err := NewLogger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l.Header(map[string]string{"model": "m"})
	l.End(2, "配置错误: base_url 未配置")

	b, err := os.ReadFile(l.Path + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		types = append(types, rec["type"].(string))
		last = rec
	}
	if len(types) != 2 || types[0] != "session_start" || types[1] != "session_end" {
		t.Fatalf("期望 session_start/session_end 成对，得到 %v", types)
	}
	if rc, _ := last["rc"].(float64); rc != 2 {
		t.Errorf("session_end 未记录退出码，得到 %v", last["rc"])
	}
	md, err := os.ReadFile(l.Path + ".md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "会话结束：rc=2") {
		t.Errorf("markdown 未记录结束行：%s", md)
	}
}
