package config

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeLocalServer(t *testing.T, healthCode int, modelsBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(healthCode)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelsBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func setLocalBase(t *testing.T, url string) {
	t.Helper()
	old := LocalLLMBase
	LocalLLMBase = url
	t.Cleanup(func() { LocalLLMBase = old })
}

func TestProbeLocalLLMReady(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `{"data":[{"id":"qwen3.5-4b-q4km"}]}`)
	setLocalBase(t, srv.URL)

	name, ok := ProbeLocalLLM()
	if !ok {
		t.Fatal("就绪的本地服务应探测成功")
	}
	if name != "qwen3.5-4b-q4km" {
		t.Fatalf("模型名 = %q，期望 qwen3.5-4b-q4km", name)
	}
}

func TestProbeLocalLLMModelListBroken(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `not-json`)
	setLocalBase(t, srv.URL)

	name, ok := ProbeLocalLLM()
	if !ok || name != "local-model" {
		t.Fatalf("模型列表不可解析时应兜底为 local-model，得到 (%q, %v)", name, ok)
	}
}

func TestProbeLocalLLMNotReady(t *testing.T) {
	// 加载中 /health 返回 503
	srv := fakeLocalServer(t, http.StatusServiceUnavailable, `{}`)
	setLocalBase(t, srv.URL)

	if _, ok := ProbeLocalLLM(); ok {
		t.Fatal("503 不应视为就绪")
	}
}

func TestProbeLocalLLMUnreachable(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `{}`)
	url := srv.URL
	srv.Close()
	setLocalBase(t, url)

	if _, ok := ProbeLocalLLM(); ok {
		t.Fatal("连接失败不应视为就绪")
	}
}

func TestUseLocalLLMTakesOver(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `{"data":[{"id":"local-x"}]}`)
	setLocalBase(t, srv.URL)

	cfg := Default()
	if !cfg.UseLocalLLM() {
		t.Fatal("探测成功应返回 true")
	}
	if !cfg.LocalInference || cfg.BaseURL != srv.URL+"/v1" || cfg.Model != "local-x" {
		t.Fatalf("本地接管结果不符: %+v", cfg)
	}
	if cfg.RequestTimeout != localMinTimeout {
		t.Fatalf("默认超时应放宽到 %d，得到 %d", localMinTimeout, cfg.RequestTimeout)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("本地接管后校验应通过: %v", err)
	}
}

func TestUseLocalLLMKeepsUserValues(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `{"data":[{"id":"local-x"}]}`)
	setLocalBase(t, srv.URL)

	cfg := Default()
	cfg.Model = "自定义名"
	cfg.RequestTimeout = 900
	if !cfg.UseLocalLLM() {
		t.Fatal("探测成功应返回 true")
	}
	if cfg.Model != "自定义名" || cfg.RequestTimeout != 900 {
		t.Fatalf("用户显式配置不应被覆盖: model=%q timeout=%d", cfg.Model, cfg.RequestTimeout)
	}
}

func TestUseLocalLLMFails(t *testing.T) {
	srv := fakeLocalServer(t, http.StatusOK, `{}`)
	url := srv.URL
	srv.Close()
	setLocalBase(t, url)

	cfg := Default()
	if cfg.UseLocalLLM() {
		t.Fatal("探测失败应返回 false")
	}
	if cfg.BaseURL != "" || cfg.LocalInference {
		t.Fatalf("探测失败不应改动配置: %+v", cfg)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("无本地且未配置 base_url 应校验失败，得到 %v", err)
	}
}

func TestValidateAllowsLocalWithoutBaseURL(t *testing.T) {
	cfg := Default()
	cfg.LocalInference = true
	cfg.BaseURL = "http://127.0.0.1:8069/v1"
	cfg.Model = "local-model"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("本地推理配置应通过校验: %v", err)
	}
}
