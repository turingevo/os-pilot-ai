package session

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"os-pilot-ai/internal/config"
	"os-pilot-ai/internal/tools"
)

// newPhaseSession 构造只带文本控制台的会话（scr 为 nil，等价于串口/无帧缓冲环境）。
func newPhaseSession(script bool) (*Session, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	cio := &consoleIO{in: bufio.NewReader(strings.NewReader("")), out: buf, script: script}
	return &Session{
		Cfg: &config.Config{Model: "Qwen3.5-4B", Mode: "orchestrate"},
		Ctx: &tools.Context{Mode: "orchestrate"},
		io:  cio,
	}, buf
}

func TestSetPhasePrintsOneDimLineOnTextConsole(t *testing.T) {
	s, buf := newPhaseSession(false)
	s.setPhase("就绪")
	if got := buf.String(); got != "  ● 就绪\n" {
		t.Errorf("文本控制台应打一行阶段提示，得到 %q", got)
	}
	if s.phase != "就绪" {
		t.Errorf("phase 未记录: %q", s.phase)
	}
}

// 脚本模式是无人值守回归，输出要和基线逐字比对，不能被阶段提示污染。
func TestSetPhaseStaysSilentInScriptMode(t *testing.T) {
	s, buf := newPhaseSession(true)
	s.setPhase("回答完毕 · 3.4s")
	if buf.Len() != 0 {
		t.Errorf("脚本模式不该有输出: %q", buf.String())
	}
	if s.phase == "" {
		t.Error("脚本模式仍应记录阶段（状态栏与其它消费方要用）")
	}
}

// 流式回答与工具调用行本身就是进度提示，文本控制台不该再多打一行。
func TestSetPhaseOnScreenIgnoresTextConsole(t *testing.T) {
	s, buf := newPhaseSession(false)
	s.setPhaseOnScreen("生成中…")
	if buf.Len() != 0 {
		t.Errorf("无帧缓冲时不该输出: %q", buf.String())
	}
	if s.phase != "" {
		t.Errorf("无帧缓冲时不该改阶段: %q", s.phase)
	}
}

func TestMarkGeneratingReportsOnlyOncePerStream(t *testing.T) {
	s, buf := newPhaseSession(false)
	s.setPhaseOnScreen("请求模型…")
	st := s.io.beginStream()
	s.markGenerating(st)
	s.markGenerating(st)
	if buf.Len() != 0 {
		t.Errorf("文本控制台不该有输出: %q", buf.String())
	}
	if !st.phaseSeen {
		t.Error("首 token 后应标记已上报")
	}
}

func TestStatusLineLeadsWithPhase(t *testing.T) {
	s, _ := newPhaseSession(false)
	s.phase = "回答完毕 · 12.4s"
	got := s.statusLine()
	if !strings.HasPrefix(got, " 回答完毕 · 12.4s │ ") {
		t.Errorf("状态栏应以阶段开头: %q", got)
	}
	if !strings.Contains(got, "Qwen3.5-4B") || !strings.Contains(got, "orchestrate") {
		t.Errorf("状态栏应保留模型与模式信息: %q", got)
	}
	s.phase = ""
	if without := s.statusLine(); !strings.HasPrefix(without, " [AI] Qwen3.5-4B") {
		t.Errorf("无阶段时不该留下悬空分隔符: %q", without)
	}
}

func TestTurnElapsed(t *testing.T) {
	s, _ := newPhaseSession(false)
	if got := s.turnElapsed(); got != "?" {
		t.Errorf("本轮未开始时应为 ?，得到 %q", got)
	}
	s.turnStart = time.Now().Add(-3400 * time.Millisecond)
	if got := s.turnElapsed(); got != "3.4s" {
		t.Errorf("整轮耗时 = %q，期望 3.4s", got)
	}
}
