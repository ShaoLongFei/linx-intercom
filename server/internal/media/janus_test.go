package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestAudioBridgePublishesAndReceivesPCMU(t *testing.T) {
	serverAudio := make(chan []byte, 1)
	clientState := make(chan webrtc.PeerConnectionState, 8)
	serverState := make(chan webrtc.PeerConnectionState, 8)
	serverPeer := make(chan *webrtc.PeerConnection, 1)
	serverTrack := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		var peer *webrtc.PeerConnection
		var remoteTrack *webrtc.TrackLocalStaticSample
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]json.RawMessage
			_ = json.Unmarshal(payload, &request)
			var kind, transaction string
			_ = json.Unmarshal(request["janus"], &kind)
			_ = json.Unmarshal(request["transaction"], &transaction)
			switch kind {
			case "create":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction, "data": map[string]any{"id": 123}})
			case "attach":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction, "data": map[string]any{"id": 456}})
			case "message":
				var body struct {
					Request string `json:"request"`
				}
				_ = json.Unmarshal(request["body"], &body)
				if body.Request == "join" {
					writeJanus(t, r.Context(), conn, map[string]any{"janus": "event", "transaction": transaction, "plugindata": map[string]any{"data": map[string]any{"audiobridge": "joined"}}})
					continue
				}
				if body.Request == "leave" {
					writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction})
					continue
				}
				var offer webrtc.SessionDescription
				_ = json.Unmarshal(request["jsep"], &offer)
				peer, err = webrtc.NewPeerConnection(webrtc.Configuration{})
				if err != nil {
					t.Errorf("create fake Janus peer: %v", err)
					return
				}
				peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) { serverState <- state })
				serverPeer <- peer
				peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
					serverTrack <- track.Codec().MimeType
					packet, _, readErr := track.ReadRTP()
					if readErr == nil {
						serverAudio <- append([]byte(nil), packet.Payload...)
					}
				})
				remoteTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "room-mix", "janus")
				if err != nil {
					t.Errorf("create remote track: %v", err)
					return
				}
				if _, err := peer.AddTrack(remoteTrack); err != nil {
					t.Errorf("add fake Janus track: %v", err)
					return
				}
				if err := peer.SetRemoteDescription(offer); err != nil {
					t.Errorf("set fake Janus remote offer: %v", err)
					return
				}
				answer, err := peer.CreateAnswer(nil)
				if err != nil {
					t.Errorf("create fake Janus answer: %v", err)
					return
				}
				if err := peer.SetLocalDescription(answer); err != nil {
					t.Errorf("set fake Janus local answer: %v", err)
					return
				}
				<-webrtc.GatheringCompletePromise(peer)
				answerJSON, _ := json.Marshal(peer.LocalDescription())
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "event", "transaction": transaction, "plugindata": map[string]any{"data": map[string]any{"audiobridge": "event"}}, "jsep": json.RawMessage(answerJSON)})
				go func() {
					select {
					case <-peerConnected(peer):
						_ = remoteTrack.WriteSample(media.Sample{Data: bytesRepeat(0xff, 160), Duration: 20 * time.Millisecond})
					case <-r.Context().Done():
					}
				}()
			case "keepalive":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction})
			case "destroy":
				if peer != nil {
					_ = peer.Close()
				}
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction})
				return
			default:
				t.Errorf("unexpected Janus request kind %q", kind)
				return
			}
		}
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	joinCtx, cancelJoin := context.WithTimeout(ctx, 10*time.Second)
	session, err := Join(joinCtx, Config{WebSocketURL: url, RoomID: 42}, "device-a")
	if err != nil {
		cancelJoin()
		t.Fatal(err)
	}
	cancelJoin()
	select {
	case <-session.janus.done:
		t.Fatal("request context cancellation closed the active media signaling session")
	case <-time.After(20 * time.Millisecond):
	}
	session.peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) { clientState <- state })
	input := make(chan []byte, 1)
	input <- make([]byte, 640)
	close(input)
	if err := session.Publish(ctx, input); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-serverAudio:
		if len(payload) != 160 {
			t.Fatalf("published RTP payload length = %d, want 160 PCMU samples", len(payload))
		}
	case <-ctx.Done():
		var remote *webrtc.PeerConnection
		select {
		case remote = <-serverPeer:
		default:
		}
		var gotTrack string
		select {
		case gotTrack = <-serverTrack:
		default:
		}
		t.Fatalf("timed out waiting for PCMU uplink RTP; client=%s/%s server=%v remote=%s/%s track=%s", session.peer.ConnectionState(), session.peer.ICEConnectionState(), drainStates(clientState), remote.ConnectionState(), remote.ICEConnectionState(), gotTrack)
	}
	select {
	case frame := <-session.Subscribe():
		if len(frame) != 640 {
			t.Fatalf("device downlink frame size = %d, want 640 PCM bytes", len(frame))
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for room mix downlink")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJoinRejectsMissingRoomBeforeConnecting(t *testing.T) {
	if _, err := Join(context.Background(), Config{WebSocketURL: "ws://127.0.0.1:1"}, "device-a"); err == nil {
		t.Fatal("zero Janus room ID accepted")
	}
}

func TestJoinRejectsJanusSignalingFailure(t *testing.T) {
	_, err := Join(context.Background(), Config{WebSocketURL: "ws://127.0.0.1:1", RoomID: 42}, "device-a")
	if err == nil {
		t.Fatal("unreachable Janus endpoint accepted")
	}
}

func TestEnsureRoomCreatesDynamicRoomOnlyOnce(t *testing.T) {
	var createCount int
	var destroyedRoomID uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, payload, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var request map[string]json.RawMessage
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Errorf("decode Janus request: %v", err)
				return
			}
			var kind, transaction string
			_ = json.Unmarshal(request["janus"], &kind)
			_ = json.Unmarshal(request["transaction"], &transaction)
			switch kind {
			case "create":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction, "data": map[string]any{"id": 123}})
			case "attach":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction, "data": map[string]any{"id": 456}})
			case "message":
				var body struct {
					Request     string `json:"request"`
					Room        uint64 `json:"room"`
					Description string `json:"description"`
					Rate        int    `json:"sampling_rate"`
					AdminKey    string `json:"admin_key"`
				}
				if err := json.Unmarshal(request["body"], &body); err != nil {
					t.Errorf("decode AudioBridge body: %v", err)
					return
				}
				data := map[string]any{"audiobridge": "success", "room": body.Room}
				if body.Request == "exists" {
					data["exists"] = createCount > 0
				} else if body.Request == "create" {
					createCount++
					if body.Room != 987 || body.Description != "Kitchen" || body.Rate != 8000 || body.AdminKey != "janus-admin" {
						t.Errorf("room create body = %+v", body)
					}
					data["audiobridge"] = "created"
				} else if body.Request == "destroy" {
					destroyedRoomID = body.Room
					data["audiobridge"] = "destroyed"
				} else {
					t.Errorf("unexpected AudioBridge request %q", body.Request)
					return
				}
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction, "plugindata": map[string]any{"data": data}})
			case "destroy":
				writeJanus(t, r.Context(), conn, map[string]any{"janus": "success", "transaction": transaction})
				return
			default:
				t.Errorf("unexpected Janus request %q", kind)
				return
			}
		}
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	config := Config{WebSocketURL: url, AdminKey: "janus-admin"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := EnsureRoom(ctx, config, 987, "Kitchen"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoom(ctx, config, 987, "Kitchen"); err != nil {
		t.Fatal(err)
	}
	if createCount != 1 {
		t.Fatalf("room create requests = %d, want 1", createCount)
	}
	if err := DestroyRoom(ctx, config, 987); err != nil {
		t.Fatal(err)
	}
	if destroyedRoomID != 987 {
		t.Fatalf("destroyed room ID = %d, want 987", destroyedRoomID)
	}
}

func drainStates(states <-chan webrtc.PeerConnectionState) []webrtc.PeerConnectionState {
	var result []webrtc.PeerConnectionState
	for {
		select {
		case state := <-states:
			result = append(result, state)
		default:
			return result
		}
	}
}

func writeJanus(t *testing.T, ctx context.Context, conn *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Errorf("encode Janus test frame: %v", err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Errorf("write Janus test frame: %v", err)
	}
}

func peerConnected(peer *webrtc.PeerConnection) <-chan struct{} {
	connected := make(chan struct{})
	if peer.ConnectionState() == webrtc.PeerConnectionStateConnected {
		close(connected)
		return connected
	}
	peer.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			select {
			case <-connected:
			default:
				close(connected)
			}
		}
	})
	return connected
}

func bytesRepeat(value byte, count int) []byte {
	data := make([]byte, count)
	for i := range data {
		data[i] = value
	}
	return data
}
