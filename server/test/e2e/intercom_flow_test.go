package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"linx-intercom/server/internal/device"
	"linx-intercom/server/internal/mcp"
	"linx-intercom/server/internal/store"
)

func TestTwoDeviceIntercomWorkflow(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/intercom.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	userA, err := db.EnsureUser("device-a")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := db.EnsureUser("device-b")
	if err != nil {
		t.Fatal(err)
	}

	sessions := device.NewSessionManager(time.Second)
	sessions.SetIdentityResolver(db.EnsureUser)
	tokens := device.TokenMap{"device-a": "token-a", "device-b": "token-b"}
	devices := httptest.NewServer(device.NewHandler(sessions, tokens))
	defer devices.Close()
	connA := connectDevice(t, devices.URL, "device-a", "token-a")
	defer connA.CloseNow()
	connB := connectDevice(t, devices.URL, "device-b", "token-b")
	defer connB.CloseNow()
	waitFor(t, func() bool { return sessions.ActiveCount() == 2 })
	initialA, initialB := sessions.Current("device-a"), sessions.Current("device-b")
	if initialA.ID == initialB.ID || initialA.UserID == initialB.UserID || initialA.LinxClientID == initialB.LinxClientID {
		t.Fatal("two devices share a session, user, or virtual Linx identity")
	}

	var joined []string
	var joinedMu sync.Mutex
	mcpHandler := mcp.NewHandler(db, sessions.IsUserOnline, "mcp-secret", mcp.RoomHooks{
		Join: func(_ context.Context, userID string, room store.Room) error {
			joinedMu.Lock()
			defer joinedMu.Unlock()
			joined = append(joined, userID+":"+room.ID)
			return nil
		},
	})
	mcpServer := httptest.NewServer(mcpHandler)
	defer mcpServer.Close()

	// 配对必须由发码方再次确认，不能把扫码/读码等同于自动互加好友。
	code := callTool(t, mcpServer.URL, "device-a", "pair_friend", map[string]any{})
	code = strings.TrimPrefix(code, "一次性配对码：")
	code = strings.Split(code, "，")[0]
	if result := callTool(t, mcpServer.URL, "device-b", "accept_friend", map[string]any{"code": code}); !strings.Contains(result, "配对请求已提交") {
		t.Fatalf("accept_friend result = %q", result)
	}
	requests := callTool(t, mcpServer.URL, "device-a", "list_pairing_requests", map[string]any{})
	var pending []store.PairingRequest
	if err := json.Unmarshal([]byte(requests), &pending); err != nil || len(pending) != 1 || pending[0].RequesterID != userB {
		t.Fatalf("pending pairings = %s, error = %v", requests, err)
	}
	if result := callTool(t, mcpServer.URL, "device-a", "confirm_friendship", map[string]any{"request_id": pending[0].ID}); !strings.Contains(result, "配对成功") {
		t.Fatalf("confirm_friendship result = %q", result)
	}
	if !db.AreFriends(userA, userB) {
		t.Fatal("friendship was not persisted after both parties confirmed")
	}

	roomText := callTool(t, mcpServer.URL, "device-a", "create_room", map[string]any{"name": "E2E Room"})
	roomID := strings.TrimPrefix(roomText, "房间已创建，编号 ")
	if roomID == roomText || roomID == "" {
		t.Fatalf("create_room result = %q", roomText)
	}
	// 邀请只针对在线设备；先断开 B 验证失败不落入待处理邀请。
	connB.CloseNow()
	waitFor(t, func() bool { return !sessions.IsUserOnline(userB) })
	if got := callTool(t, mcpServer.URL, "device-a", "invite_member", map[string]any{"room_id": roomID, "device_id": "device-b"}); !strings.Contains(got, "offline") {
		t.Fatalf("offline invite result = %q", got)
	}
	connB = connectDevice(t, devices.URL, "device-b", "token-b")
	defer connB.CloseNow()
	waitFor(t, func() bool { return sessions.IsUserOnline(userB) })
	if sessions.Current("device-b") != initialB {
		t.Fatal("reconnect within the grace period created a new session")
	}

	invitationText := callTool(t, mcpServer.URL, "device-a", "invite_member", map[string]any{"room_id": roomID, "device_id": "device-b"})
	invitationID := strings.TrimPrefix(invitationText, "邀请已发送，等待对方确认，编号 ")
	if invitationID == invitationText || invitationID == "" {
		t.Fatalf("invite_member result = %q", invitationText)
	}
	beforeAccept, err := db.RoomMembers(roomID)
	if err != nil || len(beforeAccept) != 1 || beforeAccept[0] != userA {
		t.Fatalf("unconfirmed invitation changed membership: %v, %v", beforeAccept, err)
	}
	if got := callTool(t, mcpServer.URL, "device-b", "accept_invitation", map[string]any{"invitation_id": invitationID}); got != "已加入房间" {
		t.Fatalf("accept_invitation result = %q", got)
	}

	membersText := callTool(t, mcpServer.URL, "device-a", "list_online_members", map[string]any{"room_id": roomID})
	var members []string
	if err := json.Unmarshal([]byte(membersText), &members); err != nil || len(members) != 2 {
		t.Fatalf("online members = %s, error = %v", membersText, err)
	}
	wantMembers := []string{userA, userB}
	sort.Strings(wantMembers)
	if members[0] != wantMembers[0] || members[1] != wantMembers[1] {
		t.Fatalf("online members = %v, want %v", members, wantMembers)
	}
	joinedMu.Lock()
	gotJoins := append([]string(nil), joined...)
	joinedMu.Unlock()
	if len(gotJoins) != 2 || gotJoins[0] != userA+":"+roomID || gotJoins[1] != userB+":"+roomID {
		t.Fatalf("media join hooks = %v", gotJoins)
	}
	connB.CloseNow()
	waitFor(t, func() bool { return !sessions.IsUserOnline(userB) })
	membersText = callTool(t, mcpServer.URL, "device-a", "list_online_members", map[string]any{"room_id": roomID})
	if err := json.Unmarshal([]byte(membersText), &members); err != nil || len(members) != 1 || members[0] != userA {
		t.Fatalf("offline member remained in online list: %s, %v", membersText, err)
	}
	connA.CloseNow()
	waitFor(t, func() bool { return sessions.ActiveCount() == 0 })
	select {
	case <-initialB.Done:
	case <-time.After(time.Second):
		t.Fatal("expired device session was not canceled")
	}
	if user, err := db.UserForDevice("device-b"); err != nil || user != userB || !db.AreFriends(userA, userB) {
		t.Fatalf("session expiry lost durable user or friendship: %s, %v", user, err)
	}
}

func connectDevice(t *testing.T, serverURL, deviceID, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http") + "/device/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"Device-Id":     []string{deviceID},
		"Authorization": []string{"Bearer " + token},
	}})
	if err != nil {
		t.Fatalf("connect %s: %v", deviceID, err)
	}
	return conn
}

func callTool(t *testing.T, endpoint, deviceID, name string, arguments map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint+"/mcp/message", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer mcp-secret")
	request.Header.Set("X-Linx-Device-Id", deviceID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode %s response: %v", name, err)
	}
	if response.StatusCode != http.StatusOK || len(payload.Result.Content) != 1 {
		t.Fatalf("%s response status=%d payload=%+v", name, response.StatusCode, payload)
	}
	return payload.Result.Content[0].Text
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before deadline")
}
