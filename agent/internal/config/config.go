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
	if c.BaseURL == "" {
		return fmt.Errorf("base_url 未配置")
	}
	if c.Model == "" {
		return fmt.Errorf("model 未配置")
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
