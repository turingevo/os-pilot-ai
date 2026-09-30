package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseControlListLegacyObjectForm(t *testing.T) {
	list, err := parseControlList(json.RawMessage(`{"VTOY_MENU_TIMEOUT": 30, "VTOY_DEFAULT_IMAGE": "/a.iso"}`))
	if err != nil {
		t.Fatalf("解析对象形式失败: %v", err)
	}
	if len(list) != 2 || list[0].key != "VTOY_MENU_TIMEOUT" || list[0].val != "30" {
		t.Fatalf("对象形式转换不正确: %+v", list)
	}
	if list[1].key != "VTOY_DEFAULT_IMAGE" || list[1].val != "/a.iso" {
		t.Fatalf("对象形式转换不正确: %+v", list)
	}
}

func TestParseControlListArrayForm(t *testing.T) {
	list, err := parseControlList(json.RawMessage(`[ {"A": "1"}, {"B": 2} ]`))
	if err != nil {
		t.Fatalf("解析数组形式失败: %v", err)
	}
	if len(list) != 2 || list[0].val != "1" || list[1].val != "2" {
		t.Fatalf("数组形式转换不正确: %+v", list)
	}
}

func TestControlListSetKeepsOrderAndReplaces(t *testing.T) {
	list := controlList{{key: "A", val: "1"}, {key: "B", val: "2"}}
	list = list.set("A", "9")
	list = list.set("C", "3")
	if len(list) != 3 || list[0].val != "9" || list[2].key != "C" {
		t.Fatalf("set 行为不正确: %+v", list)
	}
}

// TestScheduleBootWritesVentoyCompatibleControl 覆盖真实 bug：
// Ventoy 只识别 control 数组+字符串值，旧实现写对象+数字会被静默忽略。
func TestScheduleBootWritesVentoyCompatibleControl(t *testing.T) {
	payload := t.TempDir()
	if err := os.MkdirAll(filepath.Join(payload, "ventoy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "ubuntu.iso"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// 旧对象形式 + 数字值，且带应保留的其它键
	old := `{
    "theme": { "file": "/ventoy/theme/theme.txt" },
    "control": { "VTOY_MENU_TIMEOUT": 30 },
    "auto_install": [ { "image": "/old.iso", "template": "/scripts/old.seed", "autosel": 1, "timeout": -1 } ]
}`
	if err := os.WriteFile(filepath.Join(payload, "ventoy", "ventoy.json"), []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &Context{PayloadDir: payload, Mode: "direct", NoReboot: true}
	raw := json.RawMessage(`{"image": "/ubuntu.iso", "template": "/scripts/ubuntu.seed", "autosel": 1, "timeout": -1}`)
	if _, err := handleScheduleBoot(ctx, raw); err != nil {
		t.Fatalf("handleScheduleBoot 失败: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(payload, "ventoy", "ventoy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Theme json.RawMessage  `json:"theme"`
		Ctl   []map[string]any `json:"control"`
		AI    []map[string]any `json:"auto_install"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("写出的 ventoy.json 不是合法 JSON: %v\n%s", err, data)
	}
	if out.Theme == nil {
		t.Error("theme 键应被保留")
	}
	if len(out.Ctl) != 2 {
		t.Fatalf("control 应为 2 项数组: %s", data)
	}
	got := map[string]string{}
	for _, item := range out.Ctl {
		if len(item) != 1 {
			t.Fatalf("control 每项应只含一个键: %v", item)
		}
		for k, v := range item {
			s, ok := v.(string)
			if !ok {
				t.Fatalf("control 值必须是字符串（Ventoy 要求）: %v=%v", k, v)
			}
			got[k] = s
		}
	}
	if got["VTOY_DEFAULT_IMAGE"] != "/ubuntu.iso" || got["VTOY_MENU_TIMEOUT"] != "10" {
		t.Fatalf("control 内容不正确: %v", got)
	}
	if len(out.AI) != 2 {
		t.Fatalf("auto_install 应保留旧项并追加新项: %s", data)
	}
}

func writeScheduleVentoyJSON(t *testing.T, rawArgs string) string {
	t.Helper()
	payload := t.TempDir()
	if err := os.MkdirAll(filepath.Join(payload, "ventoy"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "ubuntu.iso"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	ctx := &Context{PayloadDir: payload, Mode: "direct", NoReboot: true}
	if _, err := handleScheduleBoot(ctx, json.RawMessage(rawArgs)); err != nil {
		t.Fatalf("handleScheduleBoot(%s) 失败: %v", rawArgs, err)
	}
	data, err := os.ReadFile(filepath.Join(payload, "ventoy", "ventoy.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestScheduleBootNeverWritesNegativeNumbers 覆盖真实 bug：
// Ventoy 的 JSON 解析器（ventoy_json.c vtoy_json_parse_value）把 '-' 交给 grub_strtoul，
// 负号解析失败会让整个 ventoy.json 失效（grub 报 "unrecognized number"），启动中断。
// 因此写入的配置里不允许出现任何负数：timeout<0 必须通过省略该键表达（插件内默认 -1）。
func TestScheduleBootNeverWritesNegativeNumbers(t *testing.T) {
	for _, args := range []string{
		`{"image": "/ubuntu.iso", "template": "/scripts/ubuntu.seed"}`,
		`{"image": "/ubuntu.iso", "template": "/scripts/ubuntu.seed", "autosel": 1, "timeout": -1}`,
		`{"image": "/ubuntu.iso", "template": "/scripts/ubuntu.seed", "autosel": 1, "timeout": 0}`,
	} {
		out := writeScheduleVentoyJSON(t, args)
		// 允许路径里的 '-'（如 0-OS-PILOT-AI），只禁止数字字面量
		for i := 0; i+1 < len(out); i++ {
			if out[i] == '-' && (out[i+1] >= '0' && out[i+1] <= '9') {
				t.Fatalf("参数 %s 写出的配置含负数（Ventoy 无法解析）:\n%s", args, out)
			}
		}
		var doc struct {
			AI []map[string]any `json:"auto_install"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("写出的 JSON 不合法: %v\n%s", err, out)
		}
		if len(doc.AI) != 1 {
			t.Fatalf("auto_install 应有 1 项: %s", out)
		}
		if _, ok := doc.AI[0]["timeout"]; ok {
			t.Fatalf("timeout<0 时应省略该键（Ventoy 插件默认即 -1）: %s", out)
		}
	}
}

// TestScheduleBootWritesPositiveTimeout 确认显式正超时仍按数字写出。
func TestScheduleBootWritesPositiveTimeout(t *testing.T) {
	out := writeScheduleVentoyJSON(t, `{"image": "/ubuntu.iso", "template": "/scripts/ubuntu.seed", "timeout": 30}`)
	var doc struct {
		AI []struct {
			Timeout *int `json:"timeout"`
		} `json:"auto_install"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("写出的 JSON 不合法: %v\n%s", err, out)
	}
	if len(doc.AI) != 1 || doc.AI[0].Timeout == nil || *doc.AI[0].Timeout != 30 {
		t.Fatalf("timeout=30 应写出数字 30: %s", out)
	}
}
