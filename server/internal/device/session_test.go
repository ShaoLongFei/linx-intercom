package device

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/audio"
)

func TestDeviceHandlerRejectsInvalidCredentialsBeforeUpgrade(t *testing.T) {
	manager := NewSessionManager(time.Minute)
	handler := NewHandler(manager, TokenMap{"device-a": "secret-a"})

	for _, tc := range []struct {
		name string
		head http.Header
		want int
	}{
		{name: "missing device id", head: http.Header{"Authorization": []string{"Bearer secret-a"}}, want: http.StatusUnauthorized},
		{name: "missing token", head: http.Header{"Device-Id": []string{"device-a"}}, want: http.StatusUnauthorized},
		{name: "wrong token", head: http.Header{"Device-Id": []string{"device-a"}, "Authorization": []string{"Bearer wrong"}}, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/device/ws", nil)
			req.Header = tc.head
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d", res.Code, tc.want)
			}
			if got := manager.ActiveCount(); got != 0 {
				t.Fatalf("invalid request allocated %d sessions", got)
			}
		})
	}
}

func TestDeviceWebSocketExchangesAudioAndControlFrames(t *testing.T) {
	manager := NewSessionManager(time.Second)
	server := httptest.NewServer(NewHandler(manager, TokenMap{"device-a": "secret-a"}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/device/ws"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"Device-Id":     []string{"device-a"},
		"Authorization": []string{"Bearer secret-a"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	var session *Session
	deadline := time.Now().Add(time.Second)
	for session == nil && time.Now().Before(deadline) {
		session = manager.Current("device-a")
		time.Sleep(time.Millisecond)
	}
	if session == nil {
		t.Fatal("websocket connection did not create a session")
	}

	if err := conn.Write(ctx, websocket.MessageBinary, []byte("pcm-frame")); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-session.AudioIn:
		if string(frame) != "pcm-frame" {
			t.Fatalf("audio frame = %q", frame)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for uplink audio")
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"listen","state":"start"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-session.ControlIn:
		if message.Type != "listen" || message.State != "start" {
			t.Fatalf("control message = %+v", message)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for control event")
	}

	if session.EnqueueAudioOut([]byte("downlink-frame")) {
		t.Fatal("unexpected downlink queue drop")
	}
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary || string(payload) != "downlink-frame" {
		t.Fatalf("downlink = (%v, %q)", messageType, payload)
	}

	session.ControlOut <- ControlMessage{Type: "mode", Mode: "control"}
	messageType, payload, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText || !strings.Contains(string(payload), `"mode":"control"`) {
		t.Fatalf("control downlink = (%v, %q)", messageType, payload)
	}

}

func TestSessionReconnectWithinGracePreservesSessionAndUser(t *testing.T) {
	manager := NewSessionManager(time.Second)
	first, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.Disconnect("device-a", generation)

	second, _, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("reconnect during grace created a different session")
	}
	if second.UserID != first.UserID {
		t.Fatalf("user id changed: got %q, want %q", second.UserID, first.UserID)
	}
	if manager.ActiveCount() != 1 {
		t.Fatalf("active session count = %d, want 1", manager.ActiveCount())
	}
}

func TestSessionRetainsPersistentLinxClientIDAcrossReconnect(t *testing.T) {
	manager := NewSessionManager(time.Second)
	manager.SetLinxClientIDResolver(func(string) (string, error) { return "550e8400-e29b-41d4-a716-446655440000", nil })
	first, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.LinxClientID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("Linx client ID = %q", first.LinxClientID)
	}
	manager.Disconnect("device-a", generation)
	resumed, _, err := manager.Attach("device-a", nil)
	if err != nil || resumed.LinxClientID != first.LinxClientID {
		t.Fatalf("Linx client ID after reconnect = %q, %v", resumed.LinxClientID, err)
	}
}

func TestSessionDoesNotInitializeWakeWordWhenLinxIdentityFails(t *testing.T) {
	manager := NewSessionManager(time.Second)
	manager.SetLinxClientIDResolver(func(string) (string, error) { return "", errors.New("database unavailable") })
	called := false
	manager.SetWakeWordFactory(func() (audio.WakeWordDetector, error) {
		called = true
		return audio.DisabledDetector{}, nil
	}, 50)
	if _, _, err := manager.Attach("device-a", nil); err == nil {
		t.Fatal("expected Linx identity resolution to fail")
	}
	if called {
		t.Fatal("wake-word resources were initialized before Linx identity resolution")
	}
}

func TestDisconnectedSessionIsOfflineDuringReconnectGrace(t *testing.T) {
	manager := NewSessionManager(time.Second)
	session, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.conn = &websocket.Conn{}
	session.mu.Unlock()
	if !manager.IsUserOnline(session.UserID) {
		t.Fatal("connected session should be online")
	}
	manager.Disconnect("device-a", generation)
	if manager.IsUserOnline(session.UserID) {
		t.Fatal("disconnected session should be offline during reconnect grace")
	}
}

func TestOnlineSessionResolvesOnlyConnectedUser(t *testing.T) {
	manager := NewSessionManager(time.Second)
	session, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.OnlineSession(session.UserID); got != nil {
		t.Fatal("disconnected session was returned as online")
	}
	session.mu.Lock()
	session.conn = &websocket.Conn{}
	session.mu.Unlock()
	if got := manager.OnlineSession(session.UserID); got != session {
		t.Fatal("connected user's session was not resolved")
	}
	manager.Disconnect("device-a", generation)
	if got := manager.OnlineSession(session.UserID); got != nil {
		t.Fatal("session in reconnect grace was returned as online")
	}
}

func TestSessionReconnectClearsStaleAudioAndControlFrames(t *testing.T) {
	manager := NewSessionManager(time.Second)
	first, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	first.EnqueueAudioOut([]byte("old room audio"))
	first.ControlOut <- ControlMessage{Type: "play_old_tts"}
	manager.Disconnect("device-a", generation)

	reconnected, _, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected != first {
		t.Fatal("reconnect during grace should preserve the runtime session")
	}
	if got := len(reconnected.AudioOut); got != 0 {
		t.Fatalf("stale audio frames after reconnect = %d, want 0", got)
	}
	if got := len(reconnected.ControlOut); got != 0 {
		t.Fatalf("stale control frames after reconnect = %d, want 0", got)
	}
}

func TestSessionExpiryReleasesRuntimeButKeepsIdentity(t *testing.T) {
	manager := NewSessionManager(15 * time.Millisecond)
	first, generation, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := first.UserID
	manager.Disconnect("device-a", generation)

	deadline := time.Now().Add(time.Second)
	for manager.ActiveCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("session was not expired: active count = %d", got)
	}

	second, _, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("new runtime session reused expired session id")
	}
	if second.UserID != userID {
		t.Fatalf("user id changed after runtime cleanup: got %q, want %q", second.UserID, userID)
	}
}

func TestStaleDisconnectDoesNotExpireReplacementSession(t *testing.T) {
	manager := NewSessionManager(time.Second)
	_, oldGeneration, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, newGeneration, err := manager.Attach("device-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if oldGeneration == newGeneration {
		t.Fatal("replacement connection did not get a new generation")
	}
	manager.Disconnect("device-a", oldGeneration)

	if manager.ActiveCount() != 1 {
		t.Fatalf("stale disconnect removed replacement: active count = %d", manager.ActiveCount())
	}
}

func TestSessionAudioQueueDropsOldestFrame(t *testing.T) {
	session := newSession("device-a", "user-a", 2)
	session.EnqueueAudioOut([]byte("one"))
	session.EnqueueAudioOut([]byte("two"))
	if !session.EnqueueAudioOut([]byte("three")) {
		t.Fatal("enqueue should report stale frame drop")
	}
	if got := string(<-session.AudioOut); got != "two" {
		t.Fatalf("first queued frame = %q, want newest retained frame %q", got, "two")
	}
	if got := string(<-session.AudioOut); got != "three" {
		t.Fatalf("second queued frame = %q, want %q", got, "three")
	}
}

func TestParseBearerToken(t *testing.T) {
	got, ok := parseBearerToken("Bearer secret-a")
	if !ok || got != "secret-a" {
		t.Fatalf("parseBearerToken() = %q, %v", got, ok)
	}
	if _, ok := parseBearerToken("Bearer " + strings.Repeat(" ", 2)); ok {
		t.Fatal("blank bearer token accepted")
	}
}
