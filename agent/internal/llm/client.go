package llm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []ToolDef `json:"tools,omitempty"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

type response struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type Client struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	MaxTokens   int
	HTTP        *http.Client
}

func NewClient(baseURL, apiKey, model string, temperature float64, maxTokens int, proxy string, timeout time.Duration) (*Client, error) {
	transport := &http.Transport{}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("http_proxy 地址无效: %w", err)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		Model:       model,
		Temperature: temperature,
		MaxTokens:   maxTokens,
		HTTP: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}, nil
}

// StreamHandler 接收流式增量；两个回调均可能为 nil。
type StreamHandler struct {
	OnReasoning func(string)
	OnContent   func(string)
}

// Chat 返回模型回复、结束原因（stop/length/tool_calls 等）与错误。
// finish_reason 供调用方区分"真的没内容"与"输出被 max_tokens 截断"。
func (c *Client) Chat(messages []Message, tools []ToolDef, h *StreamHandler) (*Message, string, error) {
	body, err := json.Marshal(request{
		Model:       c.Model,
		Messages:    messages,
		Tools:       tools,
		Temperature: c.Temperature,
		MaxTokens:   c.MaxTokens,
		Stream:      true,
	})
	if err != nil {
		return nil, "", err
	}

	req, err := http.NewRequest("POST", c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("请求大模型失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := string(data)
		if len(text) > 512 {
			text = text[:512] + "..."
		}
		return nil, "", fmt.Errorf("大模型返回 HTTP %d: %s", resp.StatusCode, text)
	}

	// 个别服务端不支持 SSE：退化为整体 JSON 解析
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, "", err
		}
		return parseFullResponse(data)
	}
	return readStream(resp.Body, h)
}

func parseFullResponse(data []byte) (*Message, string, error) {
	var rsp response
	if err := json.Unmarshal(data, &rsp); err != nil {
		return nil, "", fmt.Errorf("解析大模型响应失败: %w", err)
	}
	if rsp.Error != nil {
		return nil, "", fmt.Errorf("大模型返回错误: %s", rsp.Error.Message)
	}
	if len(rsp.Choices) == 0 {
		return nil, "", fmt.Errorf("大模型响应中没有 choices")
	}
	return &rsp.Choices[0].Message, rsp.Choices[0].FinishReason, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func readStream(r io.Reader, h *StreamHandler) (*Message, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	var content strings.Builder
	var toolCalls []ToolCall
	var finishReason string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return nil, "", fmt.Errorf("大模型返回错误: %s", ch.Error.Message)
		}
		if len(ch.Choices) == 0 {
			continue
		}
		if ch.Choices[0].FinishReason != "" {
			finishReason = ch.Choices[0].FinishReason
		}
		d := ch.Choices[0].Delta
		reasoning := d.ReasoningContent
		if reasoning == "" {
			reasoning = d.Reasoning
		}
		if reasoning != "" && h != nil && h.OnReasoning != nil {
			h.OnReasoning(reasoning)
		}
		if d.Content != "" {
			content.WriteString(d.Content)
			if h != nil && h.OnContent != nil {
				h.OnContent(d.Content)
			}
		}
		for _, tc := range d.ToolCalls {
			for len(toolCalls) <= tc.Index {
				toolCalls = append(toolCalls, ToolCall{Type: "function"})
			}
			cur := &toolCalls[tc.Index]
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return nil, "", fmt.Errorf("读取流式响应失败: %w", err)
	}
	return &Message{Role: "assistant", Content: content.String(), ToolCalls: toolCalls}, finishReason, nil
}
