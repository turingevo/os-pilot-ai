package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"syscall"

	"os-pilot-ai/internal/config"
	"os-pilot-ai/internal/llm"
	"os-pilot-ai/internal/session"
	"os-pilot-ai/internal/tools"
)

func main() {
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

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if *baseURL == "" || *model == "" {
			fmt.Fprintf(os.Stderr, "加载配置 %s 失败: %v\n", *cfgPath, err)
			fmt.Fprintf(os.Stderr, "请准备配置文件，或用 --base-url/--model/--api-key 指定大模型。\n")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "警告: %v（改用命令行参数）\n", err)
		cfg = config.Default()
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
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "配置错误: %v\n", err)
		os.Exit(2)
	}

	noReb := *noReboot || os.Getenv("VTOY_AI_NO_REBOOT") == "1"
	client, err := llm.NewClient(cfg.BaseURL, cfg.APIKey, cfg.Model, cfg.Temperature, cfg.MaxTokens, cfg.HTTPProxy, cfg.Timeout())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	reg := tools.NewDefaultRegistry()
	sess, err := session.New(cfg, client, reg, *script != "", noReb)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	sess.Ctx.Reboot = rebootNow

	if *script != "" {
		lines, err := readLines(*script)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取脚本失败: %v\n", err)
			os.Exit(1)
		}
		if err := sess.RunScript(lines); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := sess.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
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
