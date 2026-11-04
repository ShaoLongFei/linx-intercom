package linx

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestClientAuthenticatesNegotiatesAndRoutesAudio(t *testing.T) {
	// FFmpeg/libopus 生成的 440 Hz 合成音，24 kHz mono / 20 ms；没有用户录音。
	opusPacket, err := base64.StdEncoding.DecodeString("aIICsDXtEa4Hiu5g6R4d385gDeJLmQ0Pw3kC8PgaMl5qWLDEO490pQ==")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer linx-secret" || r.Header.Get("Device-Id") != "virtual-device-a" || r.Header.Get("Client-Id") != "linx-client-a" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, hello, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(hello), `"mcp":true`) {
			t.Error("advertised device MCP support without implementing its initialization/tools protocol")
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","session_id":"linx-session","audio_params":{"format":"opus","sample_rate":24000,"channels":1,"frame_duration":20}}`))
		messageType, audio, err := conn.Read(r.Context())
		if err != nil || messageType != websocket.MessageBinary || string(audio) != "spoken command" {
			t.Errorf("received audio = (%v, %q, %v)", messageType, audio, err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageBinary, opusPacket)
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"tts","state":"stop","is_aborted":false}`))
		<-r.Context().Done()
	}))
	defer server.Close()
	audioIn := make(chan []byte, 1)
	eventsIn := make(chan json.RawMessage)
	results := make(chan Result, 1)
	audioIn <- []byte("spoken command")
	ctx, cancel := context.WithCancel(context.Background())
	client := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Token: "linx-secret", ClientID: "linx-client-a"}
	result := make(chan error, 1)
	go func() { result <- client.Run(ctx, "virtual-device-a", audioIn, eventsIn, results) }()
	select {
	case output := <-results:
		if len(output.Audio) != 640 {
			t.Fatalf("decoded device PCM frame bytes = %d, want 640", len(output.Audio))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TTS audio")
	}
	var event map[string]any
	select {
	case output := <-results:
		if err := json.Unmarshal(output.Event, &event); err != nil || event["type"] != "tts" {
			t.Fatalf("TTS event = %v, %v", event, err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for TTS event")
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestClientSurfacesAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	client := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Token: "bad"}
	err := client.Run(context.Background(), "device-a", make(chan []byte), make(chan json.RawMessage), make(chan Result))
	if err == nil {
		t.Fatal("authentication failure was ignored")
	}
}

func TestClientCanNegotiateOpusInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, hello, err := conn.Read(r.Context())
		if err != nil || !strings.Contains(string(hello), `"format":"opus"`) {
			t.Errorf("opus hello = %s, %v", hello, err)
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"opus"}}`))
		messageType, audio, err := conn.Read(r.Context())
		if err != nil || messageType != websocket.MessageBinary || string(audio) != "opus-frame" {
			t.Errorf("Opus input = (%v, %q, %v)", messageType, audio, err)
		}
	}))
	defer server.Close()
	input := make(chan []byte, 1)
	input <- []byte("opus-frame")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Token: "secret", AudioFormat: "opus"}
	if err := client.Run(ctx, "device-opus", input, nil, make(chan Result)); err == nil {
		t.Fatal("unexpected clean close after server ends the negotiated session")
	}
}
