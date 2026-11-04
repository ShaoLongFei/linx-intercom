package device

import (
	"context"
	"log/slog"
	"sync"

	"linx-intercom/server/internal/media"
	"linx-intercom/server/internal/store"
)

type roomMediaBinding struct {
	roomID  string
	ctx     context.Context
	cancel  context.CancelFunc
	session *media.MediaSession
	done    chan struct{}
	once    sync.Once
}

func JoinMedia(ctx context.Context, session *Session, config media.Config, room store.Room) error {
	session.mediaJoinMu.Lock()
	defer session.mediaJoinMu.Unlock()
	if err := media.EnsureRoom(ctx, config, room.JanusID, room.Name); err != nil {
		return err
	}
	roomConfig := config
	roomConfig.RoomID = room.JanusID
	mediaSession, err := media.Join(ctx, roomConfig, session.DeviceID)
	if err != nil {
		return err
	}
	select {
	case <-session.Done:
		_ = mediaSession.Close()
		return context.Canceled
	default:
	}

	bindingCtx, cancel := context.WithCancel(context.Background())
	binding := &roomMediaBinding{roomID: room.ID, ctx: bindingCtx, cancel: cancel, session: mediaSession, done: make(chan struct{})}
	session.mediaMu.Lock()
	previous := session.media
	session.media = binding
	session.mediaMu.Unlock()
	if previous != nil {
		previous.stop()
		<-previous.done
	}
	session.mediaMu.Lock()
	session.AudioRouter.ClearRoomAudio()
	clearQueue(session.JanusAudioIn)
	clearQueue(session.AudioOut)
	session.AudioRouter.ClearJanusDownlink()
	session.mediaMu.Unlock()
	select {
	case session.ControlOut <- ControlMessage{Type: "room", State: "joined", RoomID: room.ID, Text: room.Name}:
	default:
		slog.Warn("device room status queue full", "device_id", session.DeviceID, "session_id", session.ID, "room_id", room.ID)
	}
	slog.Info("device joined Janus room", "device_id", session.DeviceID, "session_id", session.ID, "room_id", room.ID, "janus_room_id", room.JanusID)
	go runRoomMedia(session, binding)
	return nil
}

func LeaveMedia(session *Session) {
	session.mediaJoinMu.Lock()
	defer session.mediaJoinMu.Unlock()
	session.mediaMu.Lock()
	binding := session.media
	session.media = nil
	session.mediaMu.Unlock()
	if binding == nil {
		return
	}
	binding.stop()
	<-binding.done
	session.mediaMu.Lock()
	session.AudioRouter.ClearRoomAudio()
	clearQueue(session.JanusAudioIn)
	clearQueue(session.AudioOut)
	session.AudioRouter.ClearJanusDownlink()
	session.mediaMu.Unlock()
	slog.Info("device left Janus room", "device_id", session.DeviceID, "session_id", session.ID, "room_id", binding.roomID)
}

func (b *roomMediaBinding) stop() {
	b.once.Do(func() {
		b.cancel()
		if err := b.session.Close(); err != nil {
			slog.Warn("Janus media session close failed", "room_id", b.roomID, "error", err)
		}
	})
}

func runRoomMedia(session *Session, binding *roomMediaBinding) {
	defer close(binding.done)
	publishDone := make(chan error, 1)
	go func() { publishDone <- binding.session.Publish(binding.ctx, session.JanusAudioIn) }()
	for {
		select {
		case <-session.Done:
			binding.stop()
			return
		case <-binding.ctx.Done():
			return
		case err := <-publishDone:
			if binding.ctx.Err() == nil {
				slog.Warn("Janus audio publish stopped", "device_id", session.DeviceID, "session_id", session.ID, "room_id", binding.roomID, "error", err)
				binding.stop()
			}
		case frame := <-binding.session.Subscribe():
			session.AudioRouter.EnqueueJanusDownlink(frame)
		}
		if binding.ctx.Err() != nil {
			return
		}
	}
}
