package device

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
	"linx-intercom/server/internal/audio"
	"linx-intercom/server/internal/linx"
)

type wakeImmediately struct{}

func (wakeImmediately) Process([]byte) (bool, error) { return true, nil }

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
	for {
		select {
		case event := <-session.ControlOut:
			if event.Type == "tts" && event.State == "stop" {
				if !event.IsAborted || session.AudioRouter.Mode() != audio.ModeRoom || len(session.AudioOut) != 0 {
					t.Fatalf("abort did not clear playback and restore room: event=%+v queued=%d", event, len(session.AudioOut))
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("TTS abort was not handled")
		}
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
