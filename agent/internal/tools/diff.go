package tools

import (
	"fmt"
	"strings"
)

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

const maxDiffPreviewLines = 200

func UnifiedDiff(name, oldText, newText string) string {
	if oldText == newText {
		return "（内容无变化）\n"
	}
	ol, nl := splitLines(oldText), splitLines(newText)

	pre := 0
	for pre < len(ol) && pre < len(nl) && ol[pre] == nl[pre] {
		pre++
	}
	suf := 0
	for suf < len(ol)-pre && suf < len(nl)-pre && ol[len(ol)-1-suf] == nl[len(nl)-1-suf] {
		suf++
	}
	midOld, midNew := ol[pre:len(ol)-suf], nl[pre:len(nl)-suf]

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s（当前 %d 行）\n+++ %s（修改后 %d 行）\n", name, len(ol), name, len(nl))

	start := pre - 3
	if start < 0 {
		start = 0
	}
	for _, l := range ol[start:pre] {
		fmt.Fprintf(&b, "  %s\n", l)
	}

	emitted := 0
	switch {
	case len(midOld) == 0:
		for _, l := range midNew {
			if emitted >= maxDiffPreviewLines {
				b.WriteString("  ...（省略）\n")
				break
			}
			fmt.Fprintf(&b, "+ %s\n", l)
			emitted++
		}
	case len(midNew) == 0:
		for _, l := range midOld {
			if emitted >= maxDiffPreviewLines {
				b.WriteString("  ...（省略）\n")
				break
			}
			fmt.Fprintf(&b, "- %s\n", l)
			emitted++
		}
	case len(midOld)*len(midNew) <= 640000:
		for _, op := range lcsDiff(midOld, midNew) {
			if emitted >= maxDiffPreviewLines {
				b.WriteString("  ...（省略）\n")
				break
			}
			switch op.kind {
			case ' ':
				fmt.Fprintf(&b, "  %s\n", op.text)
			case '-':
				fmt.Fprintf(&b, "- %s\n", op.text)
			case '+':
				fmt.Fprintf(&b, "+ %s\n", op.text)
			}
			emitted++
		}
	default:
		fmt.Fprintf(&b, "（改动较大，仅列出差异行：旧 %d 行 → 新 %d 行）\n", len(midOld), len(midNew))
		for _, l := range midOld {
			if emitted >= 60 {
				b.WriteString("- ...（省略）\n")
				break
			}
			fmt.Fprintf(&b, "- %s\n", l)
			emitted++
		}
		for _, l := range midNew {
			if emitted >= 120 {
				b.WriteString("+ ...（省略）\n")
				break
			}
			fmt.Fprintf(&b, "+ %s\n", l)
			emitted++
		}
	}

	end := len(ol) - suf + 3
	if end > len(ol) {
		end = len(ol)
	}
	for _, l := range ol[len(ol)-suf : end] {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	return b.String()
}

type diffOp struct {
	kind byte
	text string
}

func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, diffOp{'-', a[i]})
			i++
		} else {
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}
