package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type probeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type probePayload struct {
	Root           string       `json:"root"`
	Files          []probeEntry `json:"files"`
	FileCount      int          `json:"file_count"`
	FilesShown     int          `json:"files_shown"`
	FilesTruncated bool         `json:"files_truncated"`
	Images         []probeEntry `json:"images"`
}

// runProbe 执行 system_probe 并解析出 payload 段。
func runProbe(t *testing.T, root string) (string, probePayload) {
	t.Helper()
	out, err := handleSystemProbe(&Context{PayloadDir: root}, json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("system_probe 失败: %v", err)
	}
	var info struct {
		Payload probePayload `json:"payload"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("解析探测结果失败: %v\n%s", err, out)
	}
	return out, info.Payload
}

// mkFile 按稀疏方式创建指定大小的文件（只关心清单，不关心内容）。
func mkFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
}

// 清单必须有预算：真实 U 盘塞了上百个碎片文件时，只列前 N 项，总数照实汇报。
func TestSystemProbePayloadFileListIsCapped(t *testing.T) {
	root := t.TempDir()
	const extra = payloadFileListMax*3 + 4
	for i := 0; i < extra; i++ {
		mkFile(t, filepath.Join(root, fmt.Sprintf("装机必备软件/pkg%d/依赖库/file%03d.dll", i%12, i)), 512)
	}
	mkFile(t, filepath.Join(root, "ventoy/ai/models/x1.iso"), 1<<20)
	mkFile(t, filepath.Join(root, "ventoy/ai/x2.img"), 3<<20)
	mkFile(t, filepath.Join(root, "ISO/win.WIM"), 2<<20)
	total := extra + 3

	out, p := runProbe(t, root)

	if p.FileCount != total {
		t.Errorf("file_count = %d，期望 %d", p.FileCount, total)
	}
	if len(p.Files) != payloadFileListMax {
		t.Errorf("files 条目 = %d，期望上限 %d", len(p.Files), payloadFileListMax)
	}
	if p.FilesShown != payloadFileListMax {
		t.Errorf("files_shown = %d，期望 %d", p.FilesShown, payloadFileListMax)
	}
	if !p.FilesTruncated {
		t.Error("超过上限时应标记 files_truncated")
	}
	if !strings.Contains(out, "run_command") {
		t.Error("截断时应提示模型如何看完整清单")
	}
	if len(out) > probeOutputMax {
		t.Errorf("探测结果 %d 字节，超过预算 %d", len(out), probeOutputMax)
	}
}

// 镜像清单是模型选安装源真正要的字段：大小写后缀都要认，按大小倒序。
func TestSystemProbeListsInstallImagesBySizeDesc(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "ISO/ubuntu.ISO"), 1<<20)
	mkFile(t, filepath.Join(root, "ISO/arch.img"), 3<<20)
	mkFile(t, filepath.Join(root, "ISO/win.wim"), 2<<20)
	mkFile(t, filepath.Join(root, "ISO/note.txt"), 10)
	mkFile(t, filepath.Join(root, "ISO/disk.vhdx"), 5<<20)

	_, p := runProbe(t, root)

	want := []struct {
		path string
		size int64
	}{
		{"/ISO/disk.vhdx", 5 << 20},
		{"/ISO/arch.img", 3 << 20},
		{"/ISO/win.wim", 2 << 20},
		{"/ISO/ubuntu.ISO", 1 << 20},
	}
	if len(p.Images) != len(want) {
		t.Fatalf("images = %v，期望 %d 条", p.Images, len(want))
	}
	for i, w := range want {
		if p.Images[i].Path != w.path || p.Images[i].Size != w.size {
			t.Errorf("images[%d] = %+v，期望 %+v", i, p.Images[i], w)
		}
	}
}

// 文件不多时不该出现截断痕迹。
func TestSystemProbeSmallPayloadNoTruncation(t *testing.T) {
	root := t.TempDir()
	mkFile(t, filepath.Join(root, "a.iso"), 1<<20)
	mkFile(t, filepath.Join(root, "dir/b.txt"), 0)
	mkFile(t, filepath.Join(root, "dir/c.txt"), 0)

	_, p := runProbe(t, root)

	if p.FileCount != 3 || len(p.Files) != 3 || p.FilesShown != 3 {
		t.Errorf("小目录不该截断: count=%d files=%d shown=%d", p.FileCount, len(p.Files), p.FilesShown)
	}
	if p.FilesTruncated {
		t.Error("未超过上限却标记了 files_truncated")
	}
}

// 超预算时的让位顺序：环境段（挂载/blkid）先瘦身，U 盘文件清单要保住。
// 顺序写反就等于"开发机上跑一次就看不到盘里有什么"。
func TestMarshalWithinBudgetTrimsEnvBeforeFileList(t *testing.T) {
	files := make([]any, payloadFileListMax)
	for i := range files {
		files[i] = map[string]any{"path": fmt.Sprintf("/ISO/img-%02d.iso", i), "size": 1 << 20}
	}
	env := make([]string, 60)
	for i := range env {
		env[i] = fmt.Sprintf("/dev/loop%d on /snap/very-long-package-name-%02d type squashfs (ro)", i, i)
	}
	info := map[string]any{
		"mounts": env,
		"blkid":  env,
		"fdisk":  strings.Repeat("disk table noise\n", 200),
		"payload": map[string]any{
			"root":            "/iso",
			"files":           files,
			"file_count":      300,
			"ventoy_json":     strings.Repeat(`{"control":`, 300),
			"files_shown":     payloadFileListMax,
			"files_truncated": true,
		},
	}

	out, err := marshalWithinBudget(info)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > probeOutputMax {
		t.Errorf("仍超预算: %d > %d", len(out), probeOutputMax)
	}
	var got struct {
		Mounts  []string `json:"mounts"`
		Payload struct {
			Files        []json.RawMessage `json:"files"`
			FilesDropped string            `json:"files_dropped"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("降级后输出不是合法 JSON: %v\n%s", err, out)
	}
	if len(got.Payload.Files) != payloadFileListMax {
		t.Errorf("文件清单被牺牲了（%d 条，files_dropped=%q），应先压环境段",
			len(got.Payload.Files), got.Payload.FilesDropped)
	}
	if len(got.Mounts) >= 60 {
		t.Errorf("挂载表没瘦身: %d 条", len(got.Mounts))
	}
}

// 挂载表本身也要有上限：一台装了 snap/docker 的机器能挂上百个。
func TestFilterMountsCapsList(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < mountsListMax+40; i++ {
		sb.WriteString(fmt.Sprintf("/dev/loop%d /snap/pkg%d squashfs ro 0 0\n", i, i))
	}
	got := filterMounts(sb.String())
	if len(got) != mountsListMax+1 {
		t.Fatalf("mounts 条数 = %d，期望 %d 条 + 1 条说明", len(got), mountsListMax)
	}
	if !strings.Contains(got[mountsListMax], "run_command") {
		t.Errorf("末项应是取完整信息的说明，得到 %q", got[mountsListMax])
	}
}
