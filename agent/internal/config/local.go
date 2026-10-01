package config

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// LocalLLMBase 是 init 拉起的本地 llama-server 根地址（OpenAI 兼容接口位于 /v1）。
// 变量而非常量：测试可替换为 httptest 地址。
var LocalLLMBase = "http://127.0.0.1:8069"

const (
	localProbeTimeout = 5 * time.Second
	// 本地 CPU 推理首轮 prefill 可能耗时数分钟：配置值偏小时上调，
	// 用户配置了更大的值则尊重用户。
	localMinTimeout = 1800
)

// ProbeLocalLLM 探测本机 llama-server：/health 返回 200 视为就绪，
// 随后从 /v1/models 读取模型名（读取失败时用 "local-model" 兜底）。
func ProbeLocalLLM() (string, bool) {
	client := &http.Client{Timeout: localProbeTimeout}

	resp, err := client.Get(LocalLLMBase + "/health")
	if err != nil {
		return "", false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	name := "local-model"
	if resp, err := client.Get(LocalLLMBase + "/v1/models"); err == nil {
		defer resp.Body.Close()
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&body) == nil &&
			len(body.Data) > 0 && body.Data[0].ID != "" {
			name = body.Data[0].ID
		}
	}
	return name, true
}

// UseLocalLLM 在 base_url 未配置时探测本地服务并接管配置：填 base_url、
// 补模型名、放宽超时。探测失败返回 false，配置保持原样（由 Validate 给出提示）。
func (c *Config) UseLocalLLM() bool {
	model, ok := ProbeLocalLLM()
	if !ok {
		return false
	}
	c.LocalInference = true
	c.BaseURL = LocalLLMBase + "/v1"
	if c.Model == "" {
		c.Model = model
	}
	if c.RequestTimeout < 600 {
		c.RequestTimeout = localMinTimeout
	}
	return true
}
