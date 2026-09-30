package tools

import "testing"

func TestNormalizePayloadPath(t *testing.T) {
	cases := []struct {
		root, in, want string
		wantErr        bool
	}{
		{"/iso", "/iso/ubuntu.iso", "/ubuntu.iso", false},
		{"/iso", "/ubuntu.iso", "/ubuntu.iso", false},
		{"/iso", "ubuntu.iso", "/ubuntu.iso", false},
		{"/iso", "/iso2/ubuntu.iso", "/iso2/ubuntu.iso", false},
		{"/iso", "/iso", "", true},
		{"/iso", "", "", true},
		{"/iso", "../etc/passwd", "/etc/passwd", false},
		{"/tmp/p", "/tmp/p/a/b.seed", "/a/b.seed", false},
		{"/tmp/p", "/a/b.seed", "/a/b.seed", false},
	}
	for _, c := range cases {
		ctx := &Context{PayloadDir: c.root}
		got, err := normalizePayloadPath(ctx, c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("root=%s in=%q: 期望错误，得到 %q", c.root, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("root=%s in=%q: 意外错误 %v", c.root, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("root=%s in=%q: 得到 %q，期望 %q", c.root, c.in, got, c.want)
		}
	}
}
