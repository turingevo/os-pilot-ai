package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func newOrderedObject() *orderedObject {
	return &orderedObject{vals: make(map[string]json.RawMessage)}
}

func decodeOrdered(data []byte) (*orderedObject, error) {
	o := newOrderedObject()
	if len(bytes.TrimSpace(data)) == 0 {
		return o, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("顶层不是 JSON 对象")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("JSON 键不是字符串")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.SetRaw(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *orderedObject) Get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *orderedObject) Set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o.SetRaw(key, raw)
	return nil
}

func (o *orderedObject) SetRaw(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

func (o *orderedObject) Bytes() []byte {
	return o.bytesWithIndent("")
}

// bytesWithIndent 输出对象 JSON，base 为当前嵌套层级的基础缩进。
func (o *orderedObject) bytesWithIndent(base string) []byte {
	inner := base + "    "
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range o.keys {
		b.WriteString(inner)
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteString(": ")
		val := o.vals[k]
		if len(bytes.TrimSpace(val)) == 0 {
			val = []byte("null")
		}
		b.Write(bytes.TrimRight(val, "\r\n\t "))
		if i != len(o.keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(base)
	b.WriteString("}")
	return b.Bytes()
}

type autoInstallEntry struct {
	Image    string `json:"image"`
	Template string `json:"template,omitempty"`
	Autosel  int    `json:"autosel"`
	// Timeout 只在 >=0 时输出。Ventoy 的 JSON 解析器把 '-' 开头交给 grub_strtoul
	// （ventoy_json.c vtoy_json_parse_value），负号无法解析，写出 -1 会让整个
	// ventoy.json 解析失败（grub 报 "unrecognized number"），启动直接中断。
	// 省略该键时 Ventoy 插件内的默认值就是 -1（不显示模板菜单，直接开始无人值守）。
	Timeout *int `json:"timeout,omitempty"`
}

func upsertAutoInstall(existing json.RawMessage, entry autoInstallEntry) (json.RawMessage, bool, error) {
	var items []json.RawMessage
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &items); err != nil {
			return nil, false, fmt.Errorf("ventoy.json 中 auto_install 不是数组: %w", err)
		}
	}
	replaced := false
	for i, it := range items {
		var probe struct {
			Image string `json:"image"`
		}
		if err := json.Unmarshal(it, &probe); err == nil && probe.Image == entry.Image {
			b, err := json.Marshal(entry)
			if err != nil {
				return nil, false, err
			}
			items[i] = b
			replaced = true
			break
		}
	}
	if !replaced {
		b, err := json.Marshal(entry)
		if err != nil {
			return nil, false, err
		}
		items = append(items, b)
	}

	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, it := range items {
		buf.WriteString("        ")
		buf.Write(it)
		if i != len(items)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("    ]")
	return buf.Bytes(), replaced, nil
}

type scheduleArgs struct {
	Image    string `json:"image"`
	Template string `json:"template"`
	Autosel  int    `json:"autosel"`
	Timeout  int    `json:"timeout"`
}

// controlKV 是 ventoy.json 里 control 段的一个键值对。
// Ventoy 只识别「数组 + 字符串值」形式（见 grub-core/ventoy/ventoy_plugin.c
// ventoy_plugin_control_entry），对象形式或数字值会被静默忽略，因此读写统一按规范形式。
type controlKV struct {
	key string
	val string
}

type controlList []controlKV

func parseControlList(raw json.RawMessage) (controlList, error) {
	var out controlList
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return out, nil
	}
	appendObject := func(item json.RawMessage) {
		obj, err := decodeOrdered(item)
		if err != nil {
			return
		}
		for _, k := range obj.keys {
			v := bytes.TrimSpace(obj.vals[k])
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				s = string(v)
			}
			out = append(out, controlKV{key: k, val: s})
		}
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("不是合法数组: %w", err)
		}
		for _, it := range items {
			appendObject(it)
		}
	case '{':
		// 手写/旧配置的对象形式：Ventoy 不识，读取时兼容
		appendObject(trimmed)
	default:
		return nil, fmt.Errorf("既不是数组也不是对象")
	}
	return out, nil
}

func (list controlList) set(key, val string) controlList {
	for i := range list {
		if list[i].key == key {
			list[i].val = val
			return list
		}
	}
	return append(list, controlKV{key: key, val: val})
}

func renderControlList(list controlList, base string) []byte {
	inner := base + "    "
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, kv := range list {
		kb, _ := json.Marshal(kv.key)
		vb, _ := json.Marshal(kv.val)
		b.WriteString(inner)
		b.WriteString("{ ")
		b.Write(kb)
		b.WriteString(": ")
		b.Write(vb)
		b.WriteString(" }")
		if i != len(list)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(base)
	b.WriteString("]")
	return b.Bytes()
}

func handleScheduleBoot(ctx *Context, raw json.RawMessage) (string, error) {
	var a scheduleArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}
	if !ctx.WriteAllowed() {
		return "（readonly 模式：拒绝修改启动配置）", nil
	}
	img, err := normalizePayloadPath(ctx, a.Image)
	if err != nil {
		return "", fmt.Errorf("image 无效: %w", err)
	}
	absImg := filepath.Join(ctx.PayloadRoot(), filepath.FromSlash(strings.TrimPrefix(img, "/")))
	if st, err := os.Stat(absImg); err != nil {
		return "", fmt.Errorf("镜像 %s 不存在: %w", img, err)
	} else if st.IsDir() {
		return "", fmt.Errorf("%s 是目录，不是镜像文件", img)
	}

	vjsonPath := filepath.Join(ctx.PayloadRoot(), "ventoy", "ventoy.json")
	relVjson, _ := filepath.Rel(ctx.PayloadRoot(), vjsonPath)
	oldData, err := os.ReadFile(vjsonPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("读取 %s 失败: %w", vjsonPath, err)
	}
	obj, err := decodeOrdered(oldData)
	if err != nil {
		return "", fmt.Errorf("解析 %s 失败: %w", vjsonPath, err)
	}

	var rawC json.RawMessage
	if v, ok := obj.Get("control"); ok {
		rawC = v
	}
	ctl, err := parseControlList(rawC)
	if err != nil {
		return "", fmt.Errorf("%s 的 control 段无法解析: %w", vjsonPath, err)
	}
	ctl = ctl.set("VTOY_DEFAULT_IMAGE", img)
	ctl = ctl.set("VTOY_MENU_TIMEOUT", "10")
	obj.SetRaw("control", renderControlList(ctl, "    "))

	summary := fmt.Sprintf("默认启动项 %s（菜单超时 10 秒）", img)
	tpl := strings.TrimSpace(a.Template)
	if tpl != "" {
		tpl, err = normalizePayloadPath(ctx, tpl)
		if err != nil {
			return "", fmt.Errorf("template 无效: %w", err)
		}
		sel := a.Autosel
		if sel <= 0 {
			sel = 1
		}
		to := a.Timeout
		if to == 0 {
			to = -1
		}
		var toPtr *int
		if to > 0 {
			toPtr = &to
		}
		var rawExisting json.RawMessage
		if v, ok := obj.Get("auto_install"); ok {
			rawExisting = v
		}
		rawAI, _, err := upsertAutoInstall(rawExisting, autoInstallEntry{
			Image:    img,
			Template: tpl,
			Autosel:  sel,
			Timeout:  toPtr,
		})
		if err != nil {
			return "", err
		}
		obj.SetRaw("auto_install", rawAI)
		if toPtr != nil {
			summary += fmt.Sprintf("；自动安装模板 %s（autosel=%d, timeout=%d）", tpl, sel, *toPtr)
		} else {
			summary += fmt.Sprintf("；自动安装模板 %s（autosel=%d, 不显示模板菜单）", tpl, sel)
		}
	}

	newData := obj.Bytes()
	relVjson = filepath.ToSlash(relVjson)

	if ctx.Mode != "direct" {
		diff := UnifiedDiff(relVjson, string(oldData), string(newData))
		ok, err := ctx.IO.TypedConfirm(
			fmt.Sprintf("即将写入 %s：%s", relVjson, summary),
			diff, "REBOOT")
		if err != nil {
			return "", err
		}
		if !ok {
			return "用户取消了这次配置写入。", nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(vjsonPath), 0755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	if len(oldData) > 0 {
		if err := os.WriteFile(vjsonPath+".bak", oldData, 0644); err != nil {
			return "", fmt.Errorf("备份失败: %w", err)
		}
	}
	tmp := vjsonPath + ".tmp"
	if err := os.WriteFile(tmp, newData, 0644); err != nil {
		return "", fmt.Errorf("写入失败: %w", err)
	}
	if err := os.Rename(tmp, vjsonPath); err != nil {
		return "", fmt.Errorf("替换文件失败: %w", err)
	}

	result := fmt.Sprintf("已写入 %s：%s", relVjson, summary)
	if ctx.NoReboot {
		return result + "（VTOY_AI_NO_REBOOT=1，未重启）", nil
	}
	ctx.IO.Printf("\n配置完成，3 秒后重启进入安装（再次开机将自动执行无人值守安装）...\n")
	time.Sleep(3 * time.Second)
	if ctx.Reboot != nil {
		if err := ctx.Reboot(); err != nil {
			return result + fmt.Sprintf("；但重启失败: %v", err), nil
		}
	}
	return result + "；已触发重启", nil
}
