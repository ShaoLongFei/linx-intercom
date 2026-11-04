package device

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/audio"
	"linx-intercom/server/internal/linx"
)

type wakeImmediately struct{}

func (wakeImmediately) Process([]byte) (bool, error) { return true, nil }

type frameWakeDetector struct{}

func (frameWakeDetector) Process(frame []byte) (bool, error) {
	return len(frame) > 0 && frame[0] == 2, nil
}

func TestNormalTTSStopRetainsPendingAudio(t *testing.T) {
	session := newSession("device-a", "user-a", 8)
	defer session.cancel()
	session.AudioRouter = audio.NewRouter(wakeImmediately{}, 1)
	session.AudioRouter.Process([]byte("wake"))
	for range 3 {
		session.EnqueueAudioOut(make([]byte, 640))
	}
	session.LinxOut <- linx.Result{Event: []byte(`{"type":"tts","state":"stop","is_aborted":false}`)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go forwardLinxResults(ctx, session)
	for len(session.AudioOut) == 3 && session.AudioRouter.Mode() == audio.ModeControl {
		select {
		case <-ctx.Done():
			t.Fatal("TTS stop was not handled")
		case <-time.After(time.Millisecond):
		}
	}
	if session.AudioRouter.Mode() != audio.ModeControl || len(session.AudioOut) != 4 {
		t.Fatalf("normal stop discarded pending speech or restored room too early: mode=%v queued=%d", session.AudioRouter.Mode(), len(session.AudioOut))
	}
}

func TestAbortedTTSStopClearsPendingAudio(t *testing.T) {
	session := newSession("device-a", "user-a", 8)
	defer session.cancel()
	session.AudioRouter = audio.NewRouter(wakeImmediately{}, 1)
	session.AudioRouter.Process([]byte("wake"))
	session.EnqueueAudioOut(make([]byte, 640))
	session.LinxOut <- linx.Result{Event: []byte(`{"type":"tts","state":"stop","is_aborted":true}`)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go forwardLinxResults(ctx, session)
	var aborted, restored ControlMessage
	for index := 0; index < 2; index++ {
		select {
		case event := <-session.ControlOut:
			if index == 0 {
				aborted = event
			} else {
				restored = event
			}
		case <-ctx.Done():
			t.Fatal("TTS abort or room restoration was not handled")
		}
	}
	if aborted.Type != "tts" || aborted.State != "stop" || !aborted.IsAborted {
		t.Fatalf("device must receive abort before room restoration: first event=%+v", aborted)
	}
	if restored.Type != "mode" || restored.Mode != "room" || session.AudioRouter.Mode() != audio.ModeRoom || len(session.AudioOut) != 0 {
		t.Fatalf("abort did not restore room after clearing pending playback: second event=%+v queued=%d", restored, len(session.AudioOut))
	}
}

func TestUnsolicitedPlaybackDrainDoesNotRestoreRoom(t *testing.T) {
	session := newSession("device-a", "user-a", 8)
	defer session.cancel()
	session.AudioRouter = audio.NewRouter(wakeImmediately{}, 1)
	session.AudioRouter.Process([]byte("wake"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go forwardDeviceEvents(ctx, session)
	session.ControlIn <- ControlMessage{Type: "playback", State: "drained"}
	session.ControlIn <- ControlMessage{Type: "probe"}
	select {
	case payload := <-session.LinxEventsIn:
		if !strings.Contains(string(payload), `"type":"probe"`) {
			t.Fatalf("forwarded device event = %s, want probe event", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("device event forwarding did not continue after playback acknowledgement")
	}
	if session.AudioRouter.Mode() != audio.ModeControl {
		t.Fatal("unsolicited playback-drained acknowledgement restored the room route")
	}
}

func TestOpusTTSReachesDeviceBeforeStopAndRoomRestoration(t *testing.T) {
	packet, err := base64.StdEncoding.DecodeString("aIICsDXtEa4Hiu5g6R4d385gDeJLmQ0Pw3kC8PgaMl5qWLDEO490pQ==")
	if err != nil {
		t.Fatal(err)
	}
	const expectedFrames = 72 // 超过两级 32 帧队列容量，验证突发 TTS 施加背压且不丢帧。
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"opus","sample_rate":24000,"channels":1}}`)); err != nil {
			return
		}
		// 后端必须先收到设备唤醒通知，普通房间语音不能主动触发 TTS。
		for {
			kind, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if kind == websocket.MessageText && strings.Contains(string(payload), `"detect"`) {
				break
			}
		}
		for range expectedFrames {
			if err := conn.Write(r.Context(), websocket.MessageBinary, packet); err != nil {
				return
			}
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"tts","state":"stop","is_aborted":false}`)); err != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer peer.Close()
	manager := NewSessionManager(10 * time.Millisecond)
	manager.SetWakeWordFactory(func() (audio.WakeWordDetector, error) { return wakeImmediately{}, nil }, 1)
	manager.SetOnCreate(func(session *Session) {
		StartAudioRouting(session)
		StartLinxSession(session, linx.Client{URL: "ws" + strings.TrimPrefix(peer.URL, "http"), Token: "test-linx-token"})
	})
	server := httptest.NewServer(NewHandler(manager, TokenMap{"device-a": "test-device-token"}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/device/ws", &websocket.DialOptions{HTTPHeader: http.Header{
		"Device-Id": []string{"device-a"}, "Authorization": []string{"Bearer test-device-token"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	frames, controlStarted, normalStop := 0, false, false
	for {
		kind, payload, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("device downlink failed after %d frames: %v", frames, err)
		}
		if kind == websocket.MessageBinary {
			if !controlStarted || normalStop || len(payload) != 640 {
				t.Fatalf("audio crossed mode boundary: control=%v stop=%v bytes=%d", controlStarted, normalStop, len(payload))
			}
			frames++
			continue
		}
		var event ControlMessage
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "mode" && event.Mode == "control" {
			controlStarted = true
		}
		if event.Type == "tts" && event.State == "stop" {
			if event.IsAborted || frames != expectedFrames {
				t.Fatalf("normal stop preceded complete TTS: frames=%d want=%d aborted=%v", frames, expectedFrames, event.IsAborted)
			}
			if manager.Current("device-a").AudioRouter.Mode() != audio.ModeControl {
				t.Fatal("server restored room route before device playback drained")
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"playback","state":"drained"}`)); err != nil {
				t.Fatalf("send playback-drained acknowledgement: %v", err)
			}
			normalStop = true
		}
		if event.Type == "mode" && event.Mode == "room" {
			if !normalStop || time.Since(started) < time.Second {
				t.Fatal("room restored before the paced TTS completed")
			}
			return
		}
	}
}

func TestRoomWakeLinxTTSRoomRouteEndToEnd(t *testing.T) {
	packet, err := base64.StdEncoding.DecodeString("aIICsDXtEa4Hiu5g6R4d385gDeJLmQ0Pw3kC8PgaMl5qWLDEO490pQ==")
	if err != nil {
		t.Fatal(err)
	}
	linxPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		kind, _, err := conn.Read(r.Context())
		if err != nil || kind != websocket.MessageText {
			t.Errorf("Linx hello message = %v, %v", kind, err)
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"opus","sample_rate":24000,"channels":1}}`)); err != nil {
			return
		}
		kind, event, err := conn.Read(r.Context())
		if err != nil || kind != websocket.MessageText || !strings.Contains(string(event), `"state":"detect"`) {
			t.Errorf("wake event = (%v, %s, %v)", kind, event, err)
			return
		}
		kind, command, err := conn.Read(r.Context())
		if err != nil || kind != websocket.MessageBinary || !bytes.Equal(command, bytes.Repeat([]byte{3}, 640)) {
			t.Errorf("control microphone frame = (%v, %d bytes, %v)", kind, len(command), err)
			return
		}
		for range 2 {
			if err := conn.Write(r.Context(), websocket.MessageBinary, packet); err != nil {
				return
			}
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"tts","state":"stop","is_aborted":false}`))
		<-r.Context().Done()
	}))
	defer linxPeer.Close()

	manager := NewSessionManager(time.Second)
	manager.SetWakeWordFactory(func() (audio.WakeWordDetector, error) { return frameWakeDetector{}, nil }, 1)
	manager.SetOnCreate(func(session *Session) {
		StartAudioRouting(session)
		StartLinxSession(session, linx.Client{URL: "ws" + strings.TrimPrefix(linxPeer.URL, "http"), Token: "linx-token"})
	})
	deviceServer := httptest.NewServer(NewHandler(manager, TokenMap{"device-a": "device-token"}))
	defer deviceServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deviceConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(deviceServer.URL, "http")+"/device/ws", &websocket.DialOptions{HTTPHeader: http.Header{
		"Device-Id": []string{"device-a"}, "Authorization": []string{"Bearer device-token"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer deviceConn.CloseNow()
	var session *Session
	for session == nil {
		session = manager.Current("device-a")
		if session == nil {
			select {
			case <-ctx.Done():
				t.Fatal("device session was not created")
			case <-time.After(time.Millisecond):
			}
		}
	}
	writeMicFrame := func(value byte) {
		t.Helper()
		if err := deviceConn.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte{value}, 640)); err != nil {
			t.Fatal(err)
		}
	}
	readControl := func(wantType, wantValue string) {
		t.Helper()
		kind, payload, err := deviceConn.Read(ctx)
		if err != nil || kind != websocket.MessageText {
			t.Fatalf("expected %s control event, got kind=%v payload=%q error=%v", wantType, kind, payload, err)
		}
		var event ControlMessage
		if err := json.Unmarshal(payload, &event); err != nil || event.Type != wantType {
			t.Fatalf("control event = %+v, error=%v", event, err)
		}
		if wantValue != "" && event.Mode != wantValue && event.State != wantValue {
			t.Fatalf("control event = %+v, want value %q", event, wantValue)
		}
	}

	writeMicFrame(1)
	writeMicFrame(1)
	select {
	case roomMic := <-session.JanusAudioIn:
		if !bytes.Equal(roomMic, bytes.Repeat([]byte{1}, 640)) {
			t.Fatal("ordinary microphone audio did not route to Janus unchanged")
		}
	case <-ctx.Done():
		t.Fatal("ordinary microphone audio did not reach Janus")
	}
	roomDownlink := bytes.Repeat([]byte{0xA1}, 640)
	session.AudioRouter.EnqueueJanusDownlink(roomDownlink)
	kind, payload, err := deviceConn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(payload, roomDownlink) {
		t.Fatalf("room audio downlink = (%v, %x, %v)", kind, payload[:min(len(payload), 4)], err)
	}

	writeMicFrame(2)
	readControl("mode", "control")
	select {
	case leaked := <-session.JanusAudioIn:
		t.Fatalf("wake phrase leaked to Janus: %x", leaked[:min(len(leaked), 4)])
	default:
	}
	staleRoomDownlink := bytes.Repeat([]byte{0xA2}, 640)
	session.AudioRouter.EnqueueJanusDownlink(staleRoomDownlink)
	writeMicFrame(3)
	frames := 0
	for frames < 2 {
		kind, payload, err := deviceConn.Read(ctx)
		if err != nil {
			t.Fatalf("control downlink stopped after %d frames: %v", frames, err)
		}
		if kind != websocket.MessageBinary || bytes.Equal(payload, staleRoomDownlink) {
			t.Fatalf("room audio crossed into Linx TTS mode: kind=%v payload=%x", kind, payload[:min(len(payload), 4)])
		}
		if len(payload) != 640 {
			t.Fatalf("Linx downlink frame bytes = %d, want 640", len(payload))
		}
		frames++
	}
	readControl("tts", "stop")
	if session.AudioRouter.Mode() != audio.ModeControl {
		t.Fatal("server restored room route before device playback drained")
	}
	if err := deviceConn.Write(ctx, websocket.MessageText, []byte(`{"type":"playback","state":"drained"}`)); err != nil {
		t.Fatalf("send playback-drained acknowledgement: %v", err)
	}
	readControl("mode", "room")
	roomDownlink = bytes.Repeat([]byte{0xA3}, 640)
	session.AudioRouter.EnqueueJanusDownlink(roomDownlink)
	kind, payload, err = deviceConn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || !bytes.Equal(payload, roomDownlink) {
		t.Fatalf("room audio did not resume after TTS completion: kind=%v err=%v", kind, err)
	}
}
