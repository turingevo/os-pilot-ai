package screen

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// cmdNamesDir 是补全"命令名"的来处：pack_env.sh 在 /bin 给每个 busybox applet 建同名符号链接。
// 读取失败（例如宿主环境）时退化为只补路径。
var cmdNamesDir = "/bin"

// listCmdNames 返回可补全的命令名（按名字排序）。
func listCmdNames() []string {
	ents, err := os.ReadDir(cmdNamesDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// maxCandidates 是单次补全保留的候选上限（超出的裁剪，列表里给出总数）。
const maxCandidates = 500

// maxListed 是"列出候选"写进内容区时的显示上限。
const maxListed = 120

// completeToken 计算某个词（token）的补全。
//   - cmdPos 为真表示这是行首的命令位：补 busybox applet 名；
//   - 否则按文件路径补（相对路径基于 cwd，目录补出末尾 '/'）。
//
// 返回全部候选与应当填进输入行的内容：唯一候选直接填它，多个候选填公共前缀
// （没有更长前缀时填空串，调用方保持原样并记录候选以便"再按一次 Tab 列出"）。
func completeToken(cwd, token string, cmdPos bool) (matches []string, fill string) {
	if cmdPos {
		for _, n := range listCmdNames() {
			if strings.HasPrefix(n, token) {
				matches = append(matches, n)
			}
		}
		if len(matches) == 0 { // 行首没有同名命令时退回路径补全（bash 习惯）
			matches = completePath(cwd, token)
		}
	} else {
		matches = completePath(cwd, token)
	}
	if len(matches) == 0 {
		return nil, ""
	}
	if len(matches) == 1 {
		return matches, matches[0]
	}
	if len(matches) > maxCandidates {
		matches = matches[:maxCandidates]
	}
	return matches, commonPrefix(matches)
}

// completePath 列出 dir 下前缀匹配的条目，返回可直接替换 token 的字符串。
// token 里最后一个 '/' 之前的部分原样保留（所以 "/is" → "/iso/"、"iso/ve" → "iso/ventoy/"）。
func completePath(cwd, token string) []string {
	if cwd == "" {
		cwd = "/"
	}
	dirPrefix, rest := "", token
	if i := strings.LastIndexByte(token, '/'); i >= 0 {
		dirPrefix, rest = token[:i+1], token[i+1:]
	}
	var dir string
	switch {
	case strings.HasPrefix(token, "/"):
		dir = dirPrefix
		if dir == "" {
			dir = "/"
		}
	case dirPrefix == "":
		dir = cwd
	default:
		dir = path.Join(cwd, dirPrefix)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, rest) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(rest, ".") { // 点文件要显式打点
			continue
		}
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		out = append(out, dirPrefix+name+suffix)
	}
	return out
}

// commonPrefix 返回字符串列表的最长公共前缀（按 rune 切，避免截断 UTF-8）。
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := []rune(ss[0])
	for _, s := range ss[1:] {
		r := []rune(s)
		n := 0
		for n < len(p) && n < len(r) && p[n] == r[n] {
			n++
		}
		p = p[:n]
		if len(p) == 0 {
			return ""
		}
	}
	return string(p)
}

// tokenAt 返回光标所在的"空白分隔词"范围（光标在词中时取整词）。
func tokenAt(buf []rune, cur int) (start, end int) {
	if cur < 0 {
		cur = 0
	}
	if cur > len(buf) {
		cur = len(buf)
	}
	start = cur
	for start > 0 && !isSpaceRune(buf[start-1]) {
		start--
	}
	end = cur
	for end < len(buf) && !isSpaceRune(buf[end]) {
		end++
	}
	return start, end
}

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' }

// complete 处理一次 Tab：
//   - 上一次补全有多个候选且整行未变 ⇒ 本次把候选列写入内容区；
//   - 否则重新计算：唯一候选直接补全，多候选补公共前缀并记下来（供下一次 Tab 列出）。
func (ed *lineEditor) complete() {
	s := ed.s
	if len(ed.lastComp) > 1 && string(ed.buf) == ed.lastCompLn {
		ed.listCandidates(ed.lastComp)
		ed.lastComp, ed.lastCompLn = nil, ""
		return
	}
	ed.lastComp, ed.lastCompLn = nil, ""

	start, end := tokenAt(ed.buf, ed.cur)
	token := string(ed.buf[start:end])
	cmdPos := strings.TrimSpace(string(ed.buf[:start])) == ""
	matches, fill := completeToken(s.cwd, token, cmdPos)
	if len(matches) == 0 {
		return
	}
	if fill != "" && fill != token {
		buf := append([]rune(string(ed.buf[:start])+fill), ed.buf[end:]...)
		ed.buf = buf
		ed.cur = start + len([]rune(fill))
	}
	if len(matches) > 1 {
		ed.lastComp = matches
		ed.lastCompLn = string(ed.buf)
	}
	ed.redraw()
}

// listCandidates 把候选写到内容区（bash 式：补全不动时再按一次 Tab 才列出）。
func (ed *lineEditor) listCandidates(matches []string) {
	n := len(matches)
	shown := matches
	if n > maxListed {
		shown = matches[:maxListed]
	}
	note := fmt.Sprintf("（%d 项）", n)
	if n > maxListed {
		note = fmt.Sprintf("（%d 项，只列前 %d）", n, maxListed)
	}
	s := ed.s
	savedDim, savedFg := s.dim, s.fg
	s.dim, s.fg = true, cDefault
	s.Write([]byte(note + strings.Join(shown, "  ") + "\n"))
	s.dim, s.fg = savedDim, savedFg
}
