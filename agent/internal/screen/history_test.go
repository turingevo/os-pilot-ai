package screen

import "testing"

func TestEscKeyHistVsShiftScroll(t *testing.T) {
	if k := escKey('A', nil); k.kind != keyUp {
		t.Fatalf("裸 ↑ 应为历史: %v", k.kind)
	}
	if k := escKey('B', nil); k.kind != keyDown {
		t.Fatalf("裸 ↓ 应为历史: %v", k.kind)
	}
	if k := escKey('A', []byte("1;2")); k.kind != keyScrollUp {
		t.Fatalf("Shift+↑ 应仍为回看: %v", k.kind)
	}
	if k := escKey('B', []byte("1;2")); k.kind != keyScrollDown {
		t.Fatalf("Shift+↓ 应仍为回看: %v", k.kind)
	}
}

func TestHistoryNavigationPerMode(t *testing.T) {
	s, _ := testScreen(t, 24, 4)
	s.AddHistory("ai", "帮我装系统")
	s.AddHistory("ai", "帮我装系统") // 连续重复不记
	s.AddHistory("ai", "列出镜像")
	s.AddHistory("cmd", "ls /iso")
	s.AddHistory("cmd", "df -h")

	s.SetHistoryKind("cmd")
	if line, err := readLine(t, s, "\x1b[A\r", "$ "); err != nil || line != "df -h" {
		t.Fatalf("↑ 应取最近一条命令: %q %v", line, err)
	}
	if line, _ := readLine(t, s, "\x1b[A\x1b[A\r", "$ "); line != "ls /iso" {
		t.Fatalf("↑↑ 应取更早一条: %q", line)
	}
	if line, _ := readLine(t, s, "\x1b[A\x1b[A\x1b[A\x1b[A\r", "$ "); line != "ls /iso" {
		t.Fatalf("越过最旧一条应停住: %q", line)
	}

	s.SetHistoryKind("ai")
	if line, _ := readLine(t, s, "\x1b[A\r", "你> "); line != "列出镜像" {
		t.Fatalf("AI 历史应与命令历史独立: %q", line)
	}
}

func TestHistoryDraftRestoreAndEditResets(t *testing.T) {
	s, _ := testScreen(t, 24, 4)
	s.AddHistory("cmd", "df -h")
	s.SetHistoryKind("cmd")

	// 输入 abc → ↑（存草稿）→ ↓（回草稿）→ 回车
	if line, err := readLine(t, s, "abc\x1b[A\x1b[B\r", "$ "); err != nil || line != "abc" {
		t.Fatalf("↓ 应恢复浏览前的草稿: %q %v", line, err)
	}
	// ↑ 取出历史条目后编辑，历史本身不应被改动
	if line, _ := readLine(t, s, "\x1b[Ax\r", "$ "); line != "df -hx" {
		t.Fatalf("应能在历史条目上编辑: %q", line)
	}
	if list := s.inputHist["cmd"]; len(list) != 1 || list[0] != "df -h" {
		t.Fatalf("历史不应被编辑污染: %v", list)
	}
}

func TestHistoryCap(t *testing.T) {
	s, _ := testScreen(t, 24, 4)
	for i := 0; i < histMax+5; i++ {
		s.AddHistory("cmd", "cmd-"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if n := len(s.inputHist["cmd"]); n != histMax {
		t.Fatalf("历史应封顶 %d，实际 %d", histMax, n)
	}
}
