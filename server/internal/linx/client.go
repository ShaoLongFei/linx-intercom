package linx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
)

type Client struct {
	URL         string
	Token       string
	ClientID    string
	AudioFormat string
}

type protocolMessage struct {
	Type      string `json:"type"`
	Transport string `json:"transport,omitempty"`
}

func (c Client) Run(ctx context.Context, deviceID string, audioIn <-chan []byte, audioOut chan<- []byte, eventsIn <-chan json.RawMessage, eventsOut chan<- json.RawMessage) error {
	if c.URL == "" || c.Token == "" || deviceID == "" {
		return errors.New("Linx URL, token, and device ID are required")
	}
	clientID := c.ClientID
	if clientID == "" {
		clientID = deviceID
	}
	header := http.Header{
		"Authorization":    []string{"Bearer " + c.Token},
		"Device-Id":        []string{deviceID},
		"Client-Id":        []string{clientID},
		"Protocol-Version": []string{"1"},
	}
	conn, response, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if response != nil {
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
		"type": "hello", "version": 1, "features": map[string]bool{"mcp": true}, "transport": "websocket", "audio_params": params,
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
	slog.Info("Linx virtual device connected", "device_id", deviceID)

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
			select {
			case audioOut <- append([]byte(nil), payload...):
			case <-ctx.Done():
				return nil
			default:
				slog.Warn("Linx TTS queue full; dropping stale audio", "device_id", deviceID)
			}
		case websocket.MessageText:
			select {
			case eventsOut <- append(json.RawMessage(nil), payload...):
			case <-ctx.Done():
				return nil
			default:
				slog.Warn("Linx event queue full", "device_id", deviceID)
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
