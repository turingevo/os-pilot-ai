package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// autoinstallCtx 造带序列号的靶盘 + 一个含假 ISO 的数据分区。
func autoinstallCtx(t *testing.T, mode string) *Context {
	t.Helper()
	p, _ := testProbe(t)
	// virtio 形态的序列号：<sys>/<dev>/serial
	writeF(t, filepath.Join(filepath.Dir(p.SysBlock), "devices", "vdb", "serial"), "AITARGET\n")
	payload := t.TempDir()
	if err := os.MkdirAll(filepath.Join(payload, "ISO"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeF(t, filepath.Join(payload, "ISO", "ubuntu-24.04.iso"), "fake")
	return &Context{PayloadDir: payload, Mode: mode, IO: &fakeIO{confirmOK: true}, Disks: p}
}

const genArgs = `{"image":"/ISO/ubuntu-24.04.iso","disk":"/dev/vdb","partitions":[` +
	`{"size":"16M","label":"boot-efi","mount":"/boot/efi","boot":true},` +
	`{"size":"rest","label":"root","mount":"/"}]}`

// 生成的模板必须按序列号选盘、且全文不出现设备名。
func TestGenAutoinstallUsesSerial(t *testing.T) {
	ctx := autoinstallCtx(t, "direct")
	res, err := NewDefaultRegistry().Call(ctx, "gen_autoinstall", json.RawMessage(genArgs))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK        bool   `json:"ok"`
		Path      string `json:"path"`
		IdentKind string `json:"disk_ident_kind"`
		Template  string `json:"template"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("应返回可解析 JSON: %v\n%s", err, res)
	}
	if !out.OK || out.IdentKind != "serial" {
		t.Fatalf("应成功且按 serial 选盘: %+v", out)
	}
	if !strings.Contains(out.Template, `serial: "AITARGET"`) {
		t.Fatalf("模板缺少序列号选盘:\n%s", out.Template)
	}
	if strings.Contains(out.Template, "/dev/") {
		t.Fatalf("模板不应出现设备名:\n%s", out.Template)
	}
	if _, err := os.Stat(filepath.Join(ctx.PayloadDir, out.Path)); err != nil {
		t.Fatalf("模板应已写入数据分区: %v", err)
	}
}

// readonly 一律拒绝。
func TestGenAutoinstallReadonlyRefused(t *testing.T) {
	ctx := autoinstallCtx(t, "readonly")
	res, err := NewDefaultRegistry().Call(ctx, "gen_autoinstall", json.RawMessage(genArgs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "readonly") {
		t.Fatalf("readonly 下应拒绝，实际: %s", res)
	}
}

// 镜像不存在要早报错（而不是生成一份跑不通的模板）。
func TestGenAutoinstallMissingImage(t *testing.T) {
	ctx := autoinstallCtx(t, "direct")
	args := `{"image":"/ISO/nope.iso","disk":"/dev/vdb","partitions":[{"size":"rest","label":"root","mount":"/"}]}`
	if _, err := NewDefaultRegistry().Call(ctx, "gen_autoinstall", json.RawMessage(args)); err == nil ||
		!strings.Contains(err.Error(), "镜像不存在") {
		t.Fatalf("镜像缺失应报错，实际 %v", err)
	}
}
