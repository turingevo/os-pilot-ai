package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCd(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "iso", "ventoy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "iso", "a.iso"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := resolveCd(root, ""); err != nil || got != "/" {
		t.Fatalf("无参数应回到 /: %q %v", got, err)
	}
	if got, err := resolveCd(root, "iso"); err != nil || got != filepath.Join(root, "iso") {
		t.Fatalf("相对路径应基于 cwd: %q %v", got, err)
	}
	if got, err := resolveCd(filepath.Join(root, "iso"), "ventoy"); err != nil || got != filepath.Join(root, "iso", "ventoy") {
		t.Fatalf("多级相对路径: %q %v", got, err)
	}
	if got, err := resolveCd(filepath.Join(root, "iso", "ventoy"), "../.."); err != nil || got != root {
		t.Fatalf("../ 应回到上级: %q %v", got, err)
	}
	abs := filepath.Join(root, "iso")
	if got, err := resolveCd("/", abs); err != nil || got != abs {
		t.Fatalf("绝对路径: %q %v", got, err)
	}
	if _, err := resolveCd(root, "iso/a.iso"); err == nil {
		t.Fatal("目标不是目录应报错")
	}
	if _, err := resolveCd(root, "nope"); err == nil {
		t.Fatal("目标不存在应报错")
	}
}
