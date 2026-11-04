package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := checkHealth(server.Listener.Addr().String()); err != nil {
		t.Fatalf("checkHealth() = %v", err)
	}
}

func TestParseWakeWordConfig(t *testing.T) {
	for _, name := range []string{"KWS_ENCODER", "KWS_DECODER", "KWS_JOINER", "KWS_TOKENS", "KWS_KEYWORDS_FILE", "KWS_NUM_THREADS"} {
		t.Setenv(name, "")
	}
	if _, enabled, err := parseWakeWordConfig(); err != nil || enabled {
		t.Fatalf("empty KWS config = enabled %v, error %v", enabled, err)
	}
	t.Setenv("KWS_ENCODER", "/models/encoder.onnx")
	if _, enabled, err := parseWakeWordConfig(); err == nil || enabled {
		t.Fatal("partial KWS model config was accepted")
	}
	t.Setenv("KWS_DECODER", "/models/decoder.onnx")
	t.Setenv("KWS_JOINER", "/models/joiner.onnx")
	t.Setenv("KWS_TOKENS", "/models/tokens.txt")
	t.Setenv("KWS_KEYWORDS_FILE", "/models/keywords.txt")
	t.Setenv("KWS_NUM_THREADS", "65")
	if _, enabled, err := parseWakeWordConfig(); err == nil || enabled {
		t.Fatal("out-of-range KWS thread count was accepted")
	}
	t.Setenv("KWS_NUM_THREADS", "2")
	config, enabled, err := parseWakeWordConfig()
	if err != nil || !enabled || config.NumThreads != 2 || config.KeywordsScore != 3 || config.KeywordsThreshold != 0.1 {
		t.Fatalf("valid KWS config = %+v, enabled %v, error %v", config, enabled, err)
	}
}

func TestParseDeviceTokens(t *testing.T) {
	t.Run("empty configuration is allowed for health-only development", func(t *testing.T) {
		tokens, err := parseDeviceTokens("")
		if err != nil || len(tokens) != 0 {
			t.Fatalf("parseDeviceTokens() = %v, %v", tokens, err)
		}
	})
	t.Run("valid device credentials", func(t *testing.T) {
		tokens, err := parseDeviceTokens(`{"device-a":"secret-a"}`)
		if err != nil || tokens["device-a"] != "secret-a" {
			t.Fatalf("parseDeviceTokens() = %v, %v", tokens, err)
		}
	})
	t.Run("rejects blank credentials", func(t *testing.T) {
		if _, err := parseDeviceTokens(`{"device-a":" "}`); err == nil {
			t.Fatal("blank token accepted")
		}
	})
}
