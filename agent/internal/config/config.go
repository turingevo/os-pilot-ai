package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const DefaultConfigPath = "/ventoy/ai.json"

type Config struct {
	Provider       string  `json:"provider"`
	BaseURL        string  `json:"base_url"`
	Model          string  `json:"model"`
	APIKey         string  `json:"api_key"`
	Temperature    float64 `json:"temperature"`
	MaxTokens      int     `json:"max_tokens"`
	Language       string  `json:"language"`
	Mode           string  `json:"mode"`
	HTTPProxy      string  `json:"http_proxy"`
	RequestTimeout int     `json:"request_timeout"`
	LogDir         string  `json:"log_dir"`
	PayloadDir     string  `json:"payload_dir"`

	// LocalInference 表示已探测到本地 llama-server 并接管了 base_url/model（不落配置）。
	LocalInference bool `json:"-"`
}

func Default() *Config {
	return &Config{
		Provider:       "openai-compatible",
		BaseURL:        "",
		Model:          "",
		APIKey:         "",
		Temperature:    0.2,
		MaxTokens:      262144,
		Language:       "zh-CN",
		Mode:           "orchestrate",
		HTTPProxy:      "",
		RequestTimeout: 60,
		LogDir:         "",
		PayloadDir:     "/iso",
	}
}

func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) Timeout() time.Duration {
	if c.RequestTimeout <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.RequestTimeout) * time.Second
}

func (c *Config) Validate() error {
	if c.BaseURL == "" && !c.LocalInference {
		return fmt.Errorf("base_url 未配置：请在数据分区放好本地模型（tools/fetch_local_llm.sh 可自动放置），或参考 ai.json.example 配置远程服务")
	}
	if c.Model == "" && !c.LocalInference {
		return fmt.Errorf("model 未配置：使用远程服务时需在 ai.json 指定模型名")
	}
	switch c.Mode {
	case "readonly", "orchestrate", "direct":
	default:
		return fmt.Errorf("mode 必须是 readonly/orchestrate/direct 之一，当前为 %q", c.Mode)
	}
	if c.PayloadDir == "" {
		c.PayloadDir = "/iso"
	}
	if c.LogDir == "" {
		c.LogDir = filepath.Join(c.PayloadDir, "ventoy", "ai", "logs")
	}
	return nil
}
