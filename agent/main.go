package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"os-pilot-ai/internal/config"
	"os-pilot-ai/internal/llm"
	"os-pilot-ai/internal/session"
	"os-pilot-ai/internal/tools"
)

func main() {
	os.Exit(run())
}

// run 返回进程退出码。用函数包一层是为了让 defer 生效：真机上"突然关机"事后无迹可寻，
// 所以每条退出路径都必须先把证据落到盘上（会话已建立 ⇒ session_end；尚未建立 ⇒ fatal-*.md）。
func run() (code int) {
	cfgPath := flag.String("config", config.DefaultConfigPath, "AI 配置文件路径")
	payload := flag.String("payload", "", "数据分区挂载点（覆盖配置中的 payload_dir）")
	script := flag.String("script", "", "脚本模式：从文件读取用户输入，逐条执行后退出")
	baseURL := flag.String("base-url", "", "覆盖大模型 base_url")
	model := flag.String("model", "", "覆盖模型名")
	apiKey := flag.String("api-key", "", "覆盖 API Key")
	mode := flag.String("mode", "", "覆盖安全模式 readonly/orchestrate/direct")
	logDir := flag.String("log-dir", "", "覆盖日志目录")
	noReboot := flag.Bool("no-reboot", false, "调试模式：schedule_boot 不真正重启")
	flag.Parse()

	reason := "正常结束"
	var sess *session.Session
	var stack string
	defer func() {
		// panic 的栈只会打到控制台，真机上跟着掉电一起没了：兜住它，转成盘上的证据。
		if r := recover(); r != nil {
			code = 2
			reason = fmt.Sprintf("panic: %v", r)
			stack = stackText()
			fmt.Fprintf(os.Stderr, "内部错误: %v\n%s\n", r, stack)
		}
		if sess != nil {
			if stack != "" {
				sess.Log.Note("调用栈:\n" + stack)
			}
			sess.Log.End(code, reason)
			return
		}
		// 走到这里说明会话文件从未建立：盘上只有 init 那份 boot-*.log，补一份同目录的 fatal-*.md。
		info := fatalInfo(reason, *cfgPath)
		var extra []session.FatalSection
		if stack != "" {
			extra = append(extra, session.FatalSection{Title: "调用栈", Body: stack})
		}
		if p := session.DumpFatal(fatalDir(*logDir, *payload), code, info, extra...); p != "" {
			fmt.Fprintf(os.Stderr, "诊断已写入 %s\n", strings.TrimPrefix(p, "/iso"))
		}
	}()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 无配置文件也能用：init 已尝试拉起本地模型，下面走本地探测
			fmt.Fprintf(os.Stderr, "提示: 未找到配置文件 %s，尝试使用本地模型\n", *cfgPath)
			cfg = config.Default()
		} else if *baseURL != "" && *model != "" {
			fmt.Fprintf(os.Stderr, "警告: %v（改用命令行参数）\n", err)
			cfg = config.Default()
		} else {
			reason = fmt.Sprintf("加载配置 %s 失败: %v", *cfgPath, err)
			fmt.Fprintf(os.Stderr, "%s\n", reason)
			fmt.Fprintf(os.Stderr, "请准备配置文件，或用 --base-url/--model/--api-key 指定大模型。\n")
			return 2
		}
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}
	if *model != "" {
		cfg.Model = *model
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	if *payload != "" {
		cfg.PayloadDir = *payload
	}
	if *logDir != "" {
		cfg.LogDir = *logDir
	}
	if cfg.APIKey == "" {
		if v := os.Getenv("VTOY_AI_API_KEY"); v != "" {
			cfg.APIKey = v
		}
	}
	// base_url 未配置即视为使用本地模型：探测 init 拉起的 llama-server（未就绪则降级报错）
	if cfg.BaseURL == "" {
		if cfg.UseLocalLLM() {
			fmt.Fprintf(os.Stderr, "已接入本地模型: %s（%s）\n", cfg.Model, cfg.BaseURL)
		}
	}
	if err := cfg.Validate(); err != nil {
		reason = fmt.Sprintf("配置错误: %v", err)
		fmt.Fprintf(os.Stderr, "%s\n", reason)
		return 2
	}

	noReb := *noReboot || os.Getenv("VTOY_AI_NO_REBOOT") == "1"
	client, err := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Temperature, cfg.MaxTokens, cfg.HTTPProxy, cfg.Timeout())
	if err != nil {
		reason = fmt.Sprintf("初始化大模型客户端失败: %v", err)
		fmt.Fprintf(os.Stderr, "%s\n", reason)
		return 2
	}

	reg := tools.NewDefaultRegistry()
	sess, err = session.New(cfg, client, reg, *script != "", noReb)
	if err != nil {
		reason = fmt.Sprintf("会话初始化失败: %v", err)
		fmt.Fprintf(os.Stderr, "%s\n", reason)
		sess = nil // 会话未建立，defer 走 fatal 分支
		return 1
	}
	sess.Ctx.Reboot = rebootNow

	if *script != "" {
		lines, err := readLines(*script)
		if err != nil {
			reason = fmt.Sprintf("读取脚本失败: %v", err)
			fmt.Fprintf(os.Stderr, "%s\n", reason)
			return 1
		}
		if err := sess.RunScript(lines); err != nil {
			reason = fmt.Sprintf("脚本模式结束: %v", err)
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return 1
		}
		return 0
	}
	if err := sess.Run(); err != nil {
		reason = fmt.Sprintf("会话结束: %v", err)
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

// fatalDir 在配置还没读成功（甚至文件本身损坏）时推断诊断目录：与 config.Validate
// 的默认规则一致（payload_dir 下的 ventoy/ai/logs），拿不到就退回临时目录。
func fatalDir(logDir, payloadDir string) string {
	if logDir != "" {
		return logDir
	}
	if payloadDir == "" {
		payloadDir = config.Default().PayloadDir
	}
	return filepath.Join(payloadDir, "ventoy", "ai", "logs")
}

// fatalInfo 收集诊断上下文。只列白名单：api_key 之类的敏感值绝不进这份长期留在 U 盘上的文件。
func fatalInfo(reason, cfgPath string) map[string]string {
	return map[string]string{
		"reason":     reason,
		"config":     cfgPath,
		"boot_log":   os.Getenv("VTOY_AI_BOOT_LOG"),
		"local_llm":  os.Getenv("VTOY_AI_LOCAL_LLM_REASON"),
		"initramfs":  os.Getenv("VTOY_AI_INITRAMFS"),
		"argv":       strings.Join(os.Args, " "),
		"llm_health": localLLMHealth(),
	}
}

func localLLMHealth() string {
	if model, ok := config.ProbeLocalLLM(); ok {
		return "就绪（模型 " + model + "）"
	}
	return "本地 llama-server 未就绪（" + config.LocalLLMBase + "/health 无响应）"
}

// stackText 取当前 goroutine 的调用栈；限长是因为这份内容会写进 U 盘上的诊断文件。
func stackText() string {
	buf := make([]byte, 16*1024)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func rebootNow() error {
	syscall.Sync()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}
