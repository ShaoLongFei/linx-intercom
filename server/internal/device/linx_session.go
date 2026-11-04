package device

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"linx-intercom/server/internal/linx"
)

func StartLinxSession(session *Session, client linx.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-session.Done:
			cancel()
		case <-ctx.Done():
		}
	}()
	go forwardDeviceEvents(ctx, session)
	go forwardLinxResults(ctx, session)
	go func() {
		defer cancel()
		backoff := time.Second
		for {
			err := client.Run(ctx, session.DeviceID, session.LinxAudioIn, session.LinxAudioOut, session.LinxEventsIn, session.LinxEventsOut)
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("Linx session disconnected", "device_id", session.DeviceID, "session_id", session.ID, "error", err, "retry_seconds", backoff.Seconds())
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
}

func forwardDeviceEvents(ctx context.Context, session *Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-session.ControlIn:
			payload, err := json.Marshal(message)
			if err != nil {
				slog.Error("device event encode failed", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
				continue
			}
			select {
			case session.LinxEventsIn <- payload:
			default:
				slog.Warn("Linx event queue full", "device_id", session.DeviceID, "session_id", session.ID)
			}
		}
	}
}

func forwardLinxResults(ctx context.Context, session *Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-session.LinxAudioOut:
			if session.EnqueueAudioOut(frame) {
				slog.Warn("device TTS queue dropped stale audio", "device_id", session.DeviceID, "session_id", session.ID)
			}
		case payload := <-session.LinxEventsOut:
			message, err := decodeControl(payload)
			if err != nil {
				slog.Warn("Linx control event ignored", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
				continue
			}
			select {
			case session.ControlOut <- message:
			default:
				slog.Warn("device control queue full", "device_id", session.DeviceID, "session_id", session.ID, "type", message.Type)
			}
		}
	}
}
