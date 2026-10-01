package session

import (
	"strings"
	"testing"

	"os-pilot-ai/internal/llm"
)

const probeJSON = `{"payload":{"file_count":210},"cpu":{"cores":4}}`

func TestDedupProbeReturnsShortNoteWhenUnchanged(t *testing.T) {
	s := &Session{probeInHistory: probeJSON}
	got := s.dedupProbe("system_probe", probeJSON)
	if strings.Contains(got, `"file_count"`) {
		t.Fatalf("环境未变却重复返回完整探测结果（%d 字节）", len(got))
	}
	if !strings.Contains(got, "完全相同") {
		t.Errorf("期望指针说明，得到 %q", got)
	}
	if s.probeInHistory != probeJSON {
		t.Error("未变化时不应解除去重标记")
	}
}

func TestDedupProbeReturnsFullResultWhenChanged(t *testing.T) {
	s := &Session{probeInHistory: probeJSON}
	changed := `{"payload":{"file_count":211}}`
	got := s.dedupProbe("system_probe", changed)
	if got != changed {
		t.Errorf("环境已变应返回完整新结果，得到 %q", got)
	}
	if s.probeInHistory != changed {
		t.Errorf("去重标记应更新为新结果，仍为 %q", s.probeInHistory)
	}
}

func TestDedupProbeLeavesOtherToolsAlone(t *testing.T) {
	s := &Session{probeInHistory: "same"}
	if got := s.dedupProbe("list_disks", "same"); got != "same" {
		t.Errorf("其它工具不应被去重，得到 %q", got)
	}
}

func TestForgetProbeIfTrimmedClearsWhenTextGone(t *testing.T) {
	s := &Session{
		probeInHistory: probeJSON,
		history:        []llm.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "后续提问"}},
	}
	s.forgetProbeIfTrimmed()
	if s.probeInHistory != "" {
		t.Error("探测原文已被裁掉，应解除去重标记")
	}
}

func TestForgetProbeIfTrimmedKeepsOpeningBrief(t *testing.T) {
	s := &Session{
		probeInHistory: probeJSON,
		history: []llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "【系统探测结果（自动执行 system_probe）】\n" + probeJSON},
		},
	}
	s.forgetProbeIfTrimmed()
	if s.probeInHistory == "" {
		t.Error("开场探测原文仍在历史里，不应解除标记")
	}
}

func TestForgetProbeIfTrimmedKeepsFreshToolResult(t *testing.T) {
	s := &Session{
		probeInHistory: probeJSON,
		history: []llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "tool", Name: "system_probe", Content: probeJSON},
		},
	}
	s.forgetProbeIfTrimmed()
	if s.probeInHistory == "" {
		t.Error("新的完整探测结果仍以 tool 消息在场，不应解除标记")
	}
}
