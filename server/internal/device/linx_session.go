package device

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"linx-intercom/server/internal/audio"
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
			err := client.Run(ctx, session.DeviceID, session.LinxAudioIn, session.LinxEventsIn, session.LinxOut)
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

func StartAudioRouting(session *Session) {
	go func() {
		for {
			select {
			case <-session.Done:
				return
			case frame := <-session.AudioIn:
				transition := session.AudioRouter.Process(frame)
				if transition == audio.TransitionWakeDetected {
					slog.Info("wake word detected; switching microphone route", "device_id", session.DeviceID, "session_id", session.ID, "from", "room", "to", "control")
					clearQueue(session.AudioOut)
					// 先通知设备清理旧房间播放，再触发云端回复；通知与 TTS 共用有序下行。
					select {
					case session.AudioOut <- downlinkMessage{event: &ControlMessage{Type: "mode", Mode: "control"}, mode: audio.ModeControl}:
					case <-session.Done:
						return
					}
					message, _ := json.Marshal(map[string]string{"type": "listen", "state": "detect"})
					select {
					case session.LinxEventsIn <- message:
					default:
						slog.Warn("wake event queue full", "device_id", session.DeviceID, "session_id", session.ID)
					}
				} else if transition == audio.TransitionDetectionError {
					slog.Warn("wake-word frame withheld after detector error", "device_id", session.DeviceID, "session_id", session.ID)
				}
				drainRoutedAudio(session.AudioRouter.RoomAudio(), session.JanusAudioIn, session.DeviceID, session.ID, "Janus")
				drainRoutedAudio(session.AudioRouter.LinxAudio(), session.LinxAudioIn, session.DeviceID, session.ID, "Linx")
			case frame := <-session.AudioRouter.JanusDownlink():
				if session.AudioRouter.Mode() == audio.ModeRoom && session.EnqueueAudioOut(frame) {
					slog.Warn("device room downlink queue dropped stale frame", "device_id", session.DeviceID, "session_id", session.ID)
				}
			}
		}
	}()
}

func drainRoutedAudio(source <-chan []byte, target chan []byte, deviceID, sessionID, route string) {
	for {
		select {
		case frame := <-source:
			if enqueueAudio(target, frame) {
				slog.Warn("routed microphone queue dropped stale frame", "device_id", deviceID, "session_id", sessionID, "route", route)
			}
		default:
			return
		}
	}
}

func forwardDeviceEvents(ctx context.Context, session *Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-session.ControlIn:
			if message.Type == "playback" && message.State == "drained" {
				if !session.awaitingPlaybackDrain.CompareAndSwap(true, false) {
					slog.Warn("unsolicited device playback acknowledgement ignored", "device_id", session.DeviceID, "session_id", session.ID)
					continue
				}
				completeControl(session, "device_playback_drained")
				continue
			}
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
		case result := <-session.LinxOut:
			if result.Audio != nil {
				if session.AudioRouter.Mode() != audio.ModeControl {
					continue
				}
				// 有界队列施加背压，不丢掉一段 TTS 中间的字词。
				select {
				case session.AudioOut <- downlinkMessage{audio: result.Audio, mode: audio.ModeControl}:
				case <-ctx.Done():
					return
				}
				continue
			}
			message, err := decodeControl(result.Event)
			if err != nil {
				slog.Warn("Linx control event ignored", "device_id", session.DeviceID, "session_id", session.ID, "error", err)
				continue
			}
			if message.Type == "tts" && message.State == "stop" {
				if message.IsAborted {
					clearQueue(session.AudioOut)
					select {
					case session.ControlOut <- message:
					case <-ctx.Done():
						return
					}
					completeControl(session, "tts_aborted")
					continue
				} else {
					session.awaitingPlaybackDrain.Store(true)
					select {
					case session.AudioOut <- downlinkMessage{event: &message, mode: audio.ModeControl}:
					case <-ctx.Done():
						return
					}
					continue
				}
			}
			select {
			case session.ControlOut <- message:
			default:
				slog.Warn("device control queue full", "device_id", session.DeviceID, "session_id", session.ID, "type", message.Type)
			}
		}
	}
}

func completeControl(session *Session, reason string) {
	if session.AudioRouter.CompleteControl() {
		session.awaitingPlaybackDrain.Store(false)
		clearQueue(session.AudioOut)
		slog.Info("control response completed; restoring room audio route", "device_id", session.DeviceID, "session_id", session.ID, "reason", reason)
		select {
		case session.ControlOut <- ControlMessage{Type: "mode", Mode: "room"}:
		default:
		}
	}
}
