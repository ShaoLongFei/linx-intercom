package device

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

type TokenVerifier interface {
	Verify(deviceID, token string) bool
}

type TokenMap map[string]string

func (tokens TokenMap) Verify(deviceID, token string) bool {
	expected, ok := tokens[deviceID]
	return ok && len(expected) == len(token) && subtle.ConstantTimeCompare([]byte(expected), []byte(token)) == 1
}

func NewHandler(manager *SessionManager, verifier TokenVerifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.Header.Get("Device-Id")
		token, tokenOK := parseBearerToken(r.Header.Get("Authorization"))
		if verifier == nil || !validDeviceID(deviceID) || !tokenOK || !verifier.Verify(deviceID, token) {
			http.Error(w, "unauthorized device", http.StatusUnauthorized)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			slog.Warn("device websocket upgrade failed", "device_id", deviceID, "error", err)
			return
		}
		conn.SetReadLimit(maxAudioFrameSize)
		session, generation, err := manager.Attach(deviceID, conn)
		if err != nil {
			_ = conn.Close(websocket.StatusInternalError, "session unavailable")
			slog.Error("device session allocation failed", "device_id", deviceID, "error", err)
			return
		}
		serveConnection(manager, session, generation, conn)
	})
}

func parseBearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func validDeviceID(deviceID string) bool {
	if len(deviceID) == 0 || len(deviceID) > 64 {
		return false
	}
	for _, char := range deviceID {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}

func serveConnection(manager *SessionManager, session *Session, generation uint64, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() {
		_ = conn.CloseNow()
		manager.Disconnect(session.DeviceID, generation)
	}()
	go writeMessages(ctx, session, conn)

	for {
		messageType, payload, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("device websocket read failed", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
			}
			return
		}
		switch messageType {
		case websocket.MessageBinary:
			if len(payload) == 0 || len(payload) > maxAudioFrameSize {
				slog.Warn("device audio frame rejected", "device_id", session.DeviceID, "session_id", session.ID, "size", len(payload))
				continue
			}
			if session.EnqueueAudioIn(payload) {
				slog.Warn("device audio queue dropped stale frame", "device_id", session.DeviceID, "session_id", session.ID, "direction", "uplink")
			}
		case websocket.MessageText:
			message, err := decodeControl(payload)
			if err != nil {
				slog.Warn("device control frame rejected", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
				continue
			}
			select {
			case session.ControlIn <- message:
			default:
				slog.Warn("device control queue full", "device_id", session.DeviceID, "session_id", session.ID, "type", message.Type)
			}
		}
	}
}

func writeMessages(ctx context.Context, session *Session, conn *websocket.Conn) {
	defer conn.CloseNow()
	for {
		select {
		case <-ctx.Done():
			return
		case <-session.Done:
			return
		case message := <-session.AudioOut:
			if message.mode != session.AudioRouter.Mode() {
				continue
			}
			if message.event != nil {
				if err := writeControl(ctx, session, conn, *message.event); err != nil {
					return
				}
				if message.event.Type == "tts" && message.event.State == "stop" {
					completeControl(session, "tts_drained")
				}
				continue
			}
			if err := conn.Write(ctx, websocket.MessageBinary, message.audio); err != nil {
				slog.Warn("device audio downlink failed", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
				return
			}
			// 云端 TTS 可能突发返回整句，按设备 20 ms 播放节奏发送，避免挤爆 ESP32 队列。
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-session.Done:
				timer.Stop()
				return
			case <-timer.C:
			}
		case message := <-session.ControlOut:
			if err := writeControl(ctx, session, conn, message); err != nil {
				return
			}
		}
	}
}

func writeControl(ctx context.Context, session *Session, conn *websocket.Conn, message ControlMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		slog.Warn("device control downlink failed", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
		return err
	}
	return nil
}
