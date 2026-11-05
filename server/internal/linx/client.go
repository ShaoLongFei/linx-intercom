package linx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/logctx"
	"linx-intercom/server/internal/websocketurl"
)

type Client struct {
	URL         string
	Token       string
	ClientID    string
	AudioFormat string
}

type AuthError struct {
	StatusCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("Linx websocket authentication failed with HTTP %d", e.StatusCode)
}

// 二进制音频与状态事件共用一条队列，保持 WebSocket 接收顺序。
type Result struct {
	Audio []byte
	Event json.RawMessage
}

type protocolMessage struct {
	Type        string      `json:"type"`
	Transport   string      `json:"transport,omitempty"`
	AudioParams audioParams `json:"audio_params"`
}

func (c Client) Run(ctx context.Context, deviceID string, audioIn <-chan []byte, eventsIn <-chan json.RawMessage, results chan<- Result) error {
	logger := logctx.Logger(ctx, slog.Default(), "device_id", deviceID)
	if c.URL == "" || c.Token == "" || deviceID == "" {
		return errors.New("Linx URL, token, and device ID are required")
	}
	if err := websocketurl.Validate(c.URL); err != nil {
		return fmt.Errorf("invalid Linx websocket URL: %w", err)
	}
	clientID := c.ClientID
	if clientID == "" {
		clientID = deviceID
	}
	startedAt := time.Now()
	header := http.Header{
		"Authorization":    []string{"Bearer " + c.Token},
		"Device-Id":        []string{deviceID},
		"Client-Id":        []string{clientID},
		"Protocol-Version": []string{"1"},
	}
	conn, response, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
				return fmt.Errorf("connect Linx websocket: %w", &AuthError{StatusCode: response.StatusCode})
			}
			return fmt.Errorf("connect Linx websocket: status %d", response.StatusCode)
		}
		return fmt.Errorf("connect Linx websocket: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	format := c.AudioFormat
	if format == "" {
		format = "pcm"
	}
	if format != "pcm" && format != "opus" {
		return fmt.Errorf("unsupported Linx input audio format %q", format)
	}
	params := map[string]any{"format": format, "sample_rate": 16000, "channels": 1, "frame_duration": 20}
	if format == "pcm" {
		params["bit_depth"] = 16
		params["endianness"] = "little"
		params["frame_size"] = 320
		params["sample_format"] = "signed_int16"
	}
	hello, _ := json.Marshal(map[string]any{
		// 业务工具通过云端 MCP 提供；这里没有设备侧 MCP RPC，不声明端侧 mcp 能力。
		"type": "hello", "version": 1, "transport": "websocket", "audio_params": params,
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		return fmt.Errorf("send Linx hello: %w", err)
	}
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read Linx hello: %w", err)
	}
	var helloResponse protocolMessage
	if messageType != websocket.MessageText || json.Unmarshal(payload, &helloResponse) != nil || helloResponse.Type != "hello" || helloResponse.Transport != "websocket" {
		return errors.New("invalid Linx hello response")
	}
	decoder, err := newDownlinkDecoder(helloResponse.AudioParams)
	if err != nil {
		return err
	}
	logger.Info("Linx virtual device connected", "tts_format", decoder.format, "device_pcm_sample_rate", 16000,
		"device_pcm_frame_ms", 20, "duration_ms", time.Since(startedAt).Milliseconds())

	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writeDone := make(chan error, 1)
	go writeAudio(writeCtx, conn, audioIn, eventsIn, writeDone)
	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			select {
			case writerErr := <-writeDone:
				if writerErr != nil && !errors.Is(writerErr, context.Canceled) {
					return fmt.Errorf("write Linx request: %w", writerErr)
				}
			default:
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read Linx response: %w", err)
		}
		switch messageType {
		case websocket.MessageBinary:
			frames, err := decoder.decode(payload)
			if err != nil {
				logger.Warn("Linx TTS decode failed", "format", decoder.format, "packet_bytes", len(payload), "error", err)
				return err
			}
			for _, frame := range frames {
				select {
				case results <- Result{Audio: frame}:
				case <-ctx.Done():
					return nil
				}
			}
		case websocket.MessageText:
			select {
			case results <- Result{Event: append(json.RawMessage(nil), payload...)}:
			case <-ctx.Done():
				return nil
			}
		}
	}
}

func writeAudio(ctx context.Context, conn *websocket.Conn, audioIn <-chan []byte, events <-chan json.RawMessage, done chan<- error) {
	for {
		select {
		case <-ctx.Done():
			done <- ctx.Err()
			return
		case frame, ok := <-audioIn:
			if !ok {
				audioIn = nil
				continue
			}
			if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
				done <- err
				return
			}
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := conn.Write(ctx, websocket.MessageText, event); err != nil {
				done <- err
				return
			}
		}
	}
}
