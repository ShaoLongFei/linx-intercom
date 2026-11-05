package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportSendsDeviceIdentityAndReturnsActivation(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ota/" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Activation-Version") != "1" || r.Header.Get("Device-Id") != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("activation headers = %q/%q", r.Header.Get("Activation-Version"), r.Header.Get("Device-Id"))
		}
		if r.Header.Get("Client-Id") != "client-uuid" || r.Header.Get("User-Agent") != "linx-intercom/1.2.3" {
			t.Errorf("identity headers = %q/%q", r.Header.Get("Client-Id"), r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Accept-Language") != "zh-CN" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content headers = %q/%q", r.Header.Get("Accept-Language"), r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode report body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"activation":{"code":"123456"},"websocket":{"url":"wss://linx.example/ws"}}`))
	}))
	defer server.Close()

	info := validDeviceInfo()
	client := newClient(server.URL, server.Client())
	binding, err := client.Report(context.Background(), "AA:BB:CC:DD:EE:FF", "client-uuid", "linx-intercom/1.2.3", info)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ActivationCode != "123456" || binding.WebSocketURL != "wss://linx.example/ws" || binding.WebSocketToken != "" {
		t.Fatalf("binding = %+v", binding)
	}
	if gotBody["uuid"] != "client-uuid" || gotBody["mac_address"] != "AA:BB:CC:DD:EE:FF" || gotBody["version"] != float64(0) {
		t.Fatalf("report identity = %#v", gotBody)
	}
	application, ok := gotBody["application"].(map[string]any)
	if !ok || application["version"] != "1.2.3" || application["elf_sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("application = %#v", gotBody["application"])
	}
	board, ok := gotBody["board"].(map[string]any)
	if !ok || board["type"] != "linx-intercom-esp32s3" || board["ssid"] != "test-wifi" || board["rssi"] != float64(-42) {
		t.Fatalf("board = %#v", gotBody["board"])
	}
}

func TestReportSendsSerialForActivationVersionTwo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Activation-Version") != "2" || r.Header.Get("Serial-Number") != "SERIAL-0001" {
			t.Errorf("activation headers = %q/%q", r.Header.Get("Activation-Version"), r.Header.Get("Serial-Number"))
		}
		_, _ = w.Write([]byte(`{"websocket":{"url":"wss://linx.example/ws","token":"linx-secret"}}`))
	}))
	defer server.Close()

	info := validDeviceInfo()
	info.ActivationVersion = 2
	info.SerialNumber = "SERIAL-0001"
	binding, err := newClient(server.URL, server.Client()).Report(context.Background(), "AA:BB:CC:DD:EE:FF", "client-uuid", "linx-intercom/1.2.3", info)
	if err != nil {
		t.Fatal(err)
	}
	if binding.WebSocketURL != "wss://linx.example/ws" || binding.WebSocketToken != "linx-secret" {
		t.Fatalf("binding = %+v", binding)
	}
}

func TestReportRejectsInvalidDeviceInfoBeforeRequest(t *testing.T) {
	cases := []struct {
		name  string
		mutate func(*DeviceInfo)
	}{
		{name: "device mac mismatch", mutate: func(info *DeviceInfo) { info.Report.MACAddress = "00:00:00:00:00:00" }},
		{name: "activation version unsupported", mutate: func(info *DeviceInfo) { info.ActivationVersion = 3 }},
		{name: "version one with serial", mutate: func(info *DeviceInfo) { info.SerialNumber = "SERIAL-0001" }},
		{name: "version two without serial", mutate: func(info *DeviceInfo) { info.ActivationVersion = 2 }},
		{name: "missing firmware version", mutate: func(info *DeviceInfo) { info.Report.Application.Version = "" }},
		{name: "missing firmware hash", mutate: func(info *DeviceInfo) { info.Report.Application.ELFSHA256 = "" }},
		{name: "missing board SSID", mutate: func(info *DeviceInfo) { info.Report.Board.SSID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				_, _ = w.Write([]byte(`{"activation":{"code":"123456"}}`))
			}))
			defer server.Close()

			info := validDeviceInfo()
			tc.mutate(&info)
			if _, err := newClient(server.URL, server.Client()).Report(context.Background(), "AA:BB:CC:DD:EE:FF", "client-uuid", "linx-intercom/1.2.3", info); err == nil {
				t.Fatal("invalid device info was accepted")
			}
			if called {
				t.Fatal("invalid device info reached OTA endpoint")
			}
		})
	}
}

func TestReportRejectsInvalidResponses(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "business error", statusCode: http.StatusOK, body: `{"error":"Invalid OTA request"}`},
		{name: "missing activation or credentials", statusCode: http.StatusOK, body: `{}`},
		{name: "token without URL", statusCode: http.StatusOK, body: `{"websocket":{"token":"secret"}}`},
		{name: "URL without token or activation", statusCode: http.StatusOK, body: `{"websocket":{"url":"wss://linx.example/ws"}}`},
		{name: "invalid JSON", statusCode: http.StatusOK, body: `{`},
		{name: "server failure", statusCode: http.StatusInternalServerError, body: `{"error":"internal"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			if _, err := newClient(server.URL, server.Client()).Report(context.Background(), "AA:BB:CC:DD:EE:FF", "client-uuid", "linx-intercom/1.2.3", validDeviceInfo()); err == nil {
				t.Fatal("invalid OTA response was accepted")
			}
		})
	}
}

func TestReportRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"activation":{"code":"%s"}}`, strings.Repeat("1", maxResponseBytes))
	}))
	defer server.Close()
	if _, err := newClient(server.URL, server.Client()).Report(context.Background(), "AA:BB:CC:DD:EE:FF", "client-uuid", "linx-intercom/1.2.3", validDeviceInfo()); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestCheckActivationResponses(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		bound      bool
		wantErr    bool
	}{
		{name: "pending", statusCode: http.StatusAccepted},
		{name: "bound", statusCode: http.StatusOK, body: "success", bound: true},
		{name: "business error", statusCode: http.StatusOK, body: `{"code":500,"msg":"failure"}`, wantErr: true},
		{name: "unexpected body", statusCode: http.StatusOK, body: "pending", wantErr: true},
		{name: "server failure", statusCode: http.StatusInternalServerError, body: `{"code":500}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/ota/activate" || r.Header.Get("Device-Id") != "AA:BB:CC:DD:EE:FF" {
					t.Errorf("activation request = %s %s Device-Id=%q", r.Method, r.URL.Path, r.Header.Get("Device-Id"))
				}
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			bound, err := newClient(server.URL, server.Client()).CheckActivation(context.Background(), "AA:BB:CC:DD:EE:FF")
			if (err != nil) != tc.wantErr || bound != tc.bound {
				t.Fatalf("CheckActivation() = %v, %v; want %v, error=%v", bound, err, tc.bound, tc.wantErr)
			}
		})
	}
}

func validDeviceInfo() DeviceInfo {
	return DeviceInfo{
		ActivationVersion: 1,
		Report: ReportPayload{
			MACAddress: "AA:BB:CC:DD:EE:FF",
			Application: Application{
				Name: "linx-intercom", Version: "1.2.3", CompileTime: "2026-11-05 10:00:00",
				IDFVersion: "5.3.2", ELFSHA256: strings.Repeat("a", 64),
			},
			Board: Board{Type: "linx-intercom-esp32s3", SSID: "test-wifi", RSSI: -42},
			ChipModelName: "esp32s3",
			FlashSize:     8388608,
		},
	}
}
