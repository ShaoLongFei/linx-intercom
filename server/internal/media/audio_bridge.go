package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type Config struct {
	WebSocketURL string
	APISecret    string
	RoomID       uint64
	RoomPIN      string
	ICEServers   []webrtc.ICEServer
}

type MediaSession struct {
	janus       *janusClient
	janusID     uint64
	handleID    uint64
	peer        *webrtc.PeerConnection
	localTrack  *webrtc.TrackLocalStaticSample
	deviceAudio chan []byte
	done        chan struct{}
	closeOnce   sync.Once
}

func Join(ctx context.Context, config Config, deviceID string) (*MediaSession, error) {
	if config.RoomID == 0 || deviceID == "" {
		return nil, errors.New("Janus room ID and device ID are required")
	}
	janus, sessionID, err := connectJanus(ctx, config.WebSocketURL, config.APISecret)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*MediaSession, error) {
		_ = janus.conn.CloseNow()
		return nil, err
	}
	var attached struct {
		Data struct {
			ID uint64 `json:"id"`
		} `json:"data"`
	}
	if err := janus.exchange(ctx, map[string]any{
		"janus": "attach", "session_id": sessionID, "plugin": "janus.plugin.audiobridge",
	}, &attached, nil); err != nil {
		return fail(fmt.Errorf("attach AudioBridge plugin: %w", err))
	}
	if attached.Data.ID == 0 {
		return fail(errors.New("Janus attach returned an empty handle ID"))
	}
	joinBody := map[string]any{
		"request": "join", "room": config.RoomID, "display": deviceID, "muted": false, "codec": "pcmu",
	}
	if config.RoomPIN != "" {
		joinBody["pin"] = config.RoomPIN
	}
	var joined janusEnvelope
	if err := janus.exchange(ctx, map[string]any{
		"janus": "message", "session_id": sessionID, "handle_id": attached.Data.ID, "body": joinBody,
	}, &joined, isBridgeJoined); err != nil {
		return fail(fmt.Errorf("join AudioBridge room: %w", err))
	}

	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: config.ICEServers})
	if err != nil {
		return fail(fmt.Errorf("create Janus peer connection: %w", err))
	}
	localTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "device-audio", "intercom")
	if err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("create PCMU audio track: %w", err))
	}
	if _, err := peer.AddTrack(localTrack); err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("add PCMU audio track: %w", err))
	}
	session := &MediaSession{janus: janus, janusID: sessionID, handleID: attached.Data.ID, peer: peer, localTrack: localTrack, deviceAudio: make(chan []byte, 64), done: make(chan struct{})}
	peer.OnTrack(session.receiveTrack)
	connected := make(chan struct{})
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Info("Janus WebRTC state changed", "janus_session_id", sessionID, "state", state.String())
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("create WebRTC offer: %w", err))
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("set WebRTC local offer: %w", err))
	}
	select {
	case <-webrtc.GatheringCompletePromise(peer):
	case <-ctx.Done():
		_ = peer.Close()
		return fail(ctx.Err())
	}
	jsep, _ := json.Marshal(peer.LocalDescription())
	var configured janusEnvelope
	if err := janus.exchange(ctx, map[string]any{
		"janus": "message", "session_id": sessionID, "handle_id": attached.Data.ID,
		"body": map[string]any{"request": "configure", "muted": false}, "jsep": json.RawMessage(jsep),
	}, &configured, hasJSEP); err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("configure AudioBridge media: %w", err))
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal(configured["jsep"], &answer); err != nil || answer.Type != webrtc.SDPTypeAnswer {
		_ = peer.Close()
		return fail(errors.New("Janus returned an invalid WebRTC answer"))
	}
	if err := peer.SetRemoteDescription(answer); err != nil {
		_ = peer.Close()
		return fail(fmt.Errorf("set Janus WebRTC answer: %w", err))
	}
	if peer.ConnectionState() == webrtc.PeerConnectionStateConnected {
		select {
		case <-connected:
		default:
			close(connected)
		}
	}
	select {
	case <-ctx.Done():
		_ = peer.Close()
		return fail(ctx.Err())
	case <-time.After(20 * time.Second):
		_ = peer.Close()
		return fail(fmt.Errorf("Janus WebRTC connection timeout (state %s)", peer.ConnectionState()))
	case <-connected:
	}
	go session.keepalive(ctx)
	slog.Info("Janus AudioBridge participant joined", "device_id", deviceID, "room_id", config.RoomID, "janus_session_id", sessionID)
	return session, nil
}

func (s *MediaSession) Publish(ctx context.Context, deviceAudioIn <-chan []byte) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return errors.New("media session closed")
		case pcm, ok := <-deviceAudioIn:
			if !ok {
				return nil
			}
			pcmu := pcm16ToPCMU(pcm)
			if pcmu == nil {
				continue
			}
			if err := s.localTrack.WriteSample(media.Sample{Data: pcmu, Duration: 20 * time.Millisecond}); err != nil {
				return fmt.Errorf("publish PCMU audio: %w", err)
			}
		}
	}
}

func (s *MediaSession) Subscribe() <-chan []byte { return s.deviceAudio }

func (s *MediaSession) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := s.janus.exchange(ctx, map[string]any{
			"janus": "message", "session_id": s.janusID, "handle_id": s.handleID, "body": map[string]any{"request": "leave"},
		}, nil, func(message janusEnvelope) bool {
			var kind string
			_ = json.Unmarshal(message["janus"], &kind)
			return kind == "success" || kind == "event"
		})
		if err != nil {
			closeErr = err
			slog.Warn("Janus participant leave failed", "janus_session_id", s.janusID, "error", err)
		}
		err = s.janus.exchange(ctx, map[string]any{"janus": "destroy", "session_id": s.janusID}, nil, nil)
		if err != nil && closeErr == nil {
			closeErr = err
			slog.Warn("Janus session destroy failed", "janus_session_id", s.janusID, "error", err)
		}
		if err := s.peer.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		_ = s.janus.conn.CloseNow()
		slog.Info("Janus media session released", "janus_session_id", s.janusID, "handle_id", s.handleID)
	})
	return closeErr
}

func (s *MediaSession) receiveTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if track.Codec().MimeType != webrtc.MimeTypePCMU {
		slog.Error("Janus returned unsupported audio codec", "codec", track.Codec().MimeType, "janus_session_id", s.janusID)
		return
	}
	for {
		packet, _, err := track.ReadRTP()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("Janus room audio read failed", "janus_session_id", s.janusID, "error", err)
			}
			return
		}
		pcm := pcmuToPCM16(packet.Payload)
		select {
		case s.deviceAudio <- pcm:
		case <-s.done:
			return
		default:
			select {
			case <-s.deviceAudio:
			default:
			}
			select {
			case s.deviceAudio <- pcm:
			default:
			}
			slog.Warn("Janus downlink queue dropped stale audio", "janus_session_id", s.janusID)
		}
	}
}

func (s *MediaSession) keepalive(ctx context.Context) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			keepaliveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.janus.exchange(keepaliveCtx, map[string]any{"janus": "keepalive", "session_id": s.janusID}, nil, nil)
			cancel()
			if err != nil {
				slog.Warn("Janus session keepalive failed", "janus_session_id", s.janusID, "error", err)
			}
		}
	}
}

func isBridgeJoined(message janusEnvelope) bool {
	var kind string
	_ = json.Unmarshal(message["janus"], &kind)
	if kind != "event" {
		return false
	}
	var plugin struct {
		Data struct {
			AudioBridge string `json:"audiobridge"`
		} `json:"data"`
	}
	_ = json.Unmarshal(message["plugindata"], &plugin)
	return plugin.Data.AudioBridge == "joined"
}

func hasJSEP(message janusEnvelope) bool { return len(message["jsep"]) > 0 }
