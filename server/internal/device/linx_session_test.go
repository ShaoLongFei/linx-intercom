package device

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/audio"
	"linx-intercom/server/internal/linx"
	"linx-intercom/server/internal/ota"
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

func TestMicrophoneAudioIsNotQueuedForJanusWithoutRoom(t *testing.T) {
	session := newSession("device-a", "user-a", 64)
	defer session.cancel()
	defer session.AudioRouter.Close()
	StartAudioRouting(session)
	for range 48 {
		session.EnqueueAudioIn(make([]byte, 640))
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && (len(session.AudioIn) != 0 || len(session.AudioRouter.RoomAudio()) != 0) {
		time.Sleep(time.Millisecond)
	}
	if got := len(session.JanusAudioIn); got != 0 {
		t.Fatalf("microphone frames queued for Janus without an active room: %d", got)
	}
	if got := len(session.AudioRouter.RoomAudio()); got != 0 {
		t.Fatalf("unrouted room audio remained buffered without a room: %d", got)
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
	// 该测试只验证路由，不启动真实 Janus；占位绑定表示设备已加入房间。
	session.mediaMu.Lock()
	session.media = &roomMediaBinding{}
	session.mediaMu.Unlock()
	defer func() {
		session.mediaMu.Lock()
		session.media = nil
		session.mediaMu.Unlock()
	}()
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

func TestOTADeviceSessionSpeaksPendingCodeThenConnectsWithBoundCredentials(t *testing.T) {
	const deviceID = "AA:BB:CC:DD:EE:FF"
	token := make(chan string, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token <- r.Header.Get("Authorization")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"pcm","sample_rate":16000,"channels":1}}`))
		<-r.Context().Done()
	}))
	defer peer.Close()

	var reports, checks atomic.Int32
	provider := activationTestProvider{
		report: func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error) {
			if reports.Add(1) == 1 {
				return ota.Binding{ActivationCode: "123456"}, nil
			}
			return ota.Binding{WebSocketURL: "ws" + strings.TrimPrefix(peer.URL, "http"), WebSocketToken: "linx-bound-token"}, nil
		},
		check: func(context.Context, string) (bool, error) {
			if checks.Add(1) == 1 {
				return false, errors.New("temporary OTA timeout")
			}
			return checks.Load() >= 3, nil
		},
	}
	speaker := &activationTestSpeaker{frames: [][]byte{bytes.Repeat([]byte{0x55}, 640)}, called: make(chan string, 4)}
	session := newSession(deviceID, "user-a", 64)
	defer session.cancel()
	session.LinxClientID = "persisted-client-id"
	session.DeviceInfo <- activationTestDeviceInfo(deviceID)
	startOTADeviceSession(session, provider, speaker, activationTiming{
		retryInitial: 2 * time.Millisecond, retryMax: 8 * time.Millisecond,
		pollInitial: 2 * time.Millisecond, pollMax: 8 * time.Millisecond,
		promptInterval: 100 * time.Millisecond,
	})

	select {
	case code := <-speaker.called:
		if code != "123456" {
			t.Fatalf("spoken activation code = %q", code)
		}
	case <-time.After(time.Second):
		t.Fatal("pending activation code was not spoken")
	}
	select {
	case frame := <-session.AudioOut:
		if !frame.force || !bytes.Equal(frame.audio, speaker.frames[0]) {
			t.Fatalf("activation audio downlink = %+v", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("activation audio was not queued for the device")
	}
	select {
	case got := <-token:
		if got != "Bearer linx-bound-token" {
			t.Fatalf("Linx auth header = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("bound OTA credentials did not start Linx websocket")
	}
	if reports.Load() != 2 || checks.Load() < 3 {
		t.Fatalf("OTA reports=%d checks=%d, want 2 reports and continued activation checks", reports.Load(), checks.Load())
	}
}

func TestOTADeviceSessionRefreshesCredentialsAfterLinxAuthenticationFailure(t *testing.T) {
	const deviceID = "AA:BB:CC:DD:EE:FF"
	var reports atomic.Int32
	connected := make(chan string, 2)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "Bearer expired-token" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		connected <- auth
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"pcm","sample_rate":16000,"channels":1}}`))
		<-r.Context().Done()
	}))
	defer peer.Close()
	provider := activationTestProvider{
		report: func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error) {
			if reports.Add(1) == 1 {
				return ota.Binding{WebSocketURL: "ws" + strings.TrimPrefix(peer.URL, "http"), WebSocketToken: "expired-token"}, nil
			}
			return ota.Binding{WebSocketURL: "ws" + strings.TrimPrefix(peer.URL, "http"), WebSocketToken: "fresh-token"}, nil
		},
		check: func(context.Context, string) (bool, error) { return false, errors.New("unexpected activation check") },
	}
	session := newSession(deviceID, "user-a", 8)
	defer session.cancel()
	session.LinxClientID = "client-a"
	session.DeviceInfo <- activationTestDeviceInfo(deviceID)
	startOTADeviceSession(session, provider, &activationTestSpeaker{}, shortActivationTiming())
	select {
	case auth := <-connected:
		if auth != "Bearer fresh-token" {
			t.Fatalf("connected with refreshed token = %q", auth)
		}
	case <-time.After(time.Second):
		t.Fatal("Linx auth failure did not refresh OTA credentials")
	}
	if got := reports.Load(); got != 2 {
		t.Fatalf("OTA report calls = %d, want 2", got)
	}
}

func TestOTADeviceSessionRetriesTransientLinxFailureWithSameCredentials(t *testing.T) {
	const deviceID = "AA:BB:CC:DD:EE:FF"
	var reports atomic.Int32
	var connections atomic.Int32
	secondConnection := make(chan struct{}, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stable-token" {
			t.Errorf("Linx token changed on transient reconnect: %q", r.Header.Get("Authorization"))
		}
		attempt := connections.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","transport":"websocket","audio_params":{"format":"pcm","sample_rate":16000,"channels":1}}`))
		if attempt == 1 {
			return
		}
		secondConnection <- struct{}{}
		<-r.Context().Done()
	}))
	defer peer.Close()
	provider := activationTestProvider{
		report: func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error) {
			reports.Add(1)
			return ota.Binding{WebSocketURL: "ws" + strings.TrimPrefix(peer.URL, "http"), WebSocketToken: "stable-token"}, nil
		},
		check: func(context.Context, string) (bool, error) { return false, errors.New("unexpected activation check") },
	}
	session := newSession(deviceID, "user-a", 8)
	defer session.cancel()
	session.LinxClientID = "client-a"
	session.DeviceInfo <- activationTestDeviceInfo(deviceID)
	startOTADeviceSession(session, provider, &activationTestSpeaker{}, shortActivationTiming())
	select {
	case <-secondConnection:
	case <-time.After(time.Second):
		t.Fatal("transient Linx failure was not retried")
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("OTA report calls = %d, want to reuse current credentials", got)
	}
}

func TestOTADeviceSessionsKeepActivationAndAudioIsolated(t *testing.T) {
	type result struct {
		deviceID string
		code     string
		frame    byte
	}
	results := make(chan result, 2)
	sessions := []*Session{
		newSession("AA:BB:CC:DD:EE:01", "user-a", 8),
		newSession("AA:BB:CC:DD:EE:02", "user-b", 8),
	}
	codes := map[string]string{
		sessions[0].DeviceID: "111111",
		sessions[1].DeviceID: "222222",
	}
	for _, session := range sessions {
		session := session
		session.DeviceInfo <- activationTestDeviceInfo(session.DeviceID)
		provider := activationTestProvider{
			report: func(_ context.Context, deviceID, _, _ string, _ ota.DeviceInfo) (ota.Binding, error) {
				return ota.Binding{ActivationCode: codes[deviceID]}, nil
			},
			check: func(context.Context, string) (bool, error) { return false, nil },
		}
		speaker := &activationTestSpeaker{
			frames: [][]byte{bytes.Repeat([]byte{byte(len(codes[session.DeviceID])) + session.DeviceID[len(session.DeviceID)-1]}, 640)},
			called: make(chan string, 1),
		}
		go func() {
			code := <-speaker.called
			frame := <-session.AudioOut
			results <- result{deviceID: session.DeviceID, code: code, frame: frame.audio[0]}
		}()
		startOTADeviceSession(session, provider, speaker, shortActivationTiming())
		startOTADeviceSession(session, provider, speaker, shortActivationTiming())
	}
	defer sessions[0].cancel()
	defer sessions[1].cancel()
	seen := make(map[string]result)
	for range 2 {
		select {
		case got := <-results:
			seen[got.deviceID] = got
		case <-time.After(time.Second):
			t.Fatal("device did not receive its own activation prompt")
		}
	}
	for _, session := range sessions {
		got := seen[session.DeviceID]
		wantCode := codes[session.DeviceID]
		wantFrame := byte(len(wantCode)) + session.DeviceID[len(session.DeviceID)-1]
		if got.code != wantCode || got.frame != wantFrame {
			t.Fatalf("device %s received code=%s frame=%x, want code=%s frame=%x", session.DeviceID, got.code, got.frame, wantCode, wantFrame)
		}
	}
}

func TestActivationSessionCancelsPollingWhenSessionExpires(t *testing.T) {
	const deviceID = "AA:BB:CC:DD:EE:FF"
	checkStarted := make(chan struct{}, 1)
	checkCanceled := make(chan struct{}, 1)
	provider := activationTestProvider{
		report: func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error) {
			return ota.Binding{ActivationCode: "123456"}, nil
		},
		check: func(ctx context.Context, _ string) (bool, error) {
			checkStarted <- struct{}{}
			<-ctx.Done()
			checkCanceled <- struct{}{}
			return false, ctx.Err()
		},
	}
	session := newSession(deviceID, "user-a", 8)
	session.DeviceInfo <- activationTestDeviceInfo(deviceID)
	startOTADeviceSession(session, provider, &activationTestSpeaker{}, shortActivationTiming())
	select {
	case <-checkStarted:
	case <-time.After(time.Second):
		t.Fatal("activation polling did not start")
	}
	session.cancel()
	select {
	case <-checkCanceled:
	case <-time.After(time.Second):
		t.Fatal("session expiry did not cancel activation polling")
	}
}

func TestActivationSpeechFailureRetriesWithoutEndingDeviceSession(t *testing.T) {
	const deviceID = "AA:BB:CC:DD:EE:FF"
	provider := activationTestProvider{
		report: func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error) {
			return ota.Binding{ActivationCode: "123456"}, nil
		},
		check: func(context.Context, string) (bool, error) { return false, nil },
	}
	speaker := &activationTestSpeaker{
		frames:    [][]byte{bytes.Repeat([]byte{0x33}, 640)},
		called:    make(chan string, 4),
		failFirst: true,
	}
	session := newSession(deviceID, "user-a", 8)
	defer session.cancel()
	session.DeviceInfo <- activationTestDeviceInfo(deviceID)
	timing := shortActivationTiming()
	timing.promptInterval = 10 * time.Millisecond
	startOTADeviceSession(session, provider, speaker, timing)
	for range 2 {
		select {
		case code := <-speaker.called:
			if code != "123456" {
				t.Fatalf("activation code = %q", code)
			}
		case <-time.After(time.Second):
			t.Fatal("activation speech was not retried")
		}
	}
	select {
	case frame := <-session.AudioOut:
		if !bytes.Equal(frame.audio, speaker.frames[0]) {
			t.Fatalf("retried activation frame = %x", frame.audio[:min(4, len(frame.audio))])
		}
	case <-time.After(time.Second):
		t.Fatal("retried activation prompt did not reach device queue")
	}
	select {
	case <-session.Done:
		t.Fatal("speech failure ended the device session")
	default:
	}
}

func TestActivationBackoffDefaultsMatchProductTiming(t *testing.T) {
	if defaultActivationTiming.retryInitial != time.Second || defaultActivationTiming.retryMax != 30*time.Second ||
		defaultActivationTiming.pollInitial != 5*time.Second || defaultActivationTiming.pollMax != 30*time.Second ||
		defaultActivationTiming.promptInterval != 60*time.Second {
		t.Fatalf("activation timing defaults = %+v", defaultActivationTiming)
	}
}

func shortActivationTiming() activationTiming {
	return activationTiming{
		retryInitial: 2 * time.Millisecond, retryMax: 8 * time.Millisecond,
		pollInitial: 2 * time.Millisecond, pollMax: 8 * time.Millisecond,
		promptInterval: 20 * time.Millisecond,
	}
}

type activationTestProvider struct {
	report func(context.Context, string, string, string, ota.DeviceInfo) (ota.Binding, error)
	check  func(context.Context, string) (bool, error)
}

func (p activationTestProvider) Report(ctx context.Context, deviceID, clientID, userAgent string, info ota.DeviceInfo) (ota.Binding, error) {
	return p.report(ctx, deviceID, clientID, userAgent, info)
}

func (p activationTestProvider) CheckActivation(ctx context.Context, deviceID string) (bool, error) {
	return p.check(ctx, deviceID)
}

type activationTestSpeaker struct {
	frames    [][]byte
	called    chan string
	err       error
	failFirst bool
	attempts  atomic.Int32
}

func (s *activationTestSpeaker) Synthesize(_ context.Context, code string) ([][]byte, error) {
	if s.called != nil {
		s.called <- code
	}
	if s.failFirst && s.attempts.Add(1) == 1 {
		return nil, errors.New("temporary TTS failure")
	}
	return s.frames, s.err
}

func activationTestDeviceInfo(deviceID string) ota.DeviceInfo {
	return ota.DeviceInfo{
		ActivationVersion: 1,
		Report: ota.ReportPayload{
			MACAddress:  deviceID,
			Application: ota.Application{Name: "linx-intercom", Version: "1.0.0", ELFSHA256: strings.Repeat("a", 64)},
			Board:       ota.Board{Type: "linx-intercom-esp32s3", SSID: "test-wifi", RSSI: -40},
		},
	}
}
