package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"linx-intercom/server/internal/store"
)

func TestCreateRoomUsesLinxDeviceHeaderIdentity(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/mcp.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	caller, _ := db.EnsureUser("linx-device-a")
	other, _ := db.EnsureUser("linx-device-b")
	handler := NewHandler(db, func(userID string) bool { return userID == caller }, "mcp-secret")
	request := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"create_room","arguments":{"name":"Studio","user_id":"` + other + `"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/message", bytes.NewBufferString(request))
	req.Header.Set("Authorization", "Bearer mcp-secret")
	req.Header.Set("X-Linx-Device-Id", "linx-device-a")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var response rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.IsError {
		t.Fatalf("tool result is error: %+v", response.Result)
	}
	rooms, err := db.RoomsForUser(caller)
	if err != nil || len(rooms) != 1 || rooms[0].OwnerID != caller {
		t.Fatalf("rooms for caller = %+v, %v", rooms, err)
	}
	rooms, err = db.RoomsForUser(other)
	if err != nil || len(rooms) != 0 {
		t.Fatalf("model-selected identity unexpectedly owns room: %+v, %v", rooms, err)
	}
}

func TestMCPToolsListAndAuthorization(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, _ = db.EnsureUser("device-a")
	handler := NewHandler(db, func(string) bool { return false }, "secret")
	unauthorized := httptest.NewRequest(http.MethodPost, "/mcp/message", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	unauthorized.Header.Set("X-Linx-Device-Id", "device-a")
	unauthorizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedRecorder.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp/message", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Linx-Device-Id", "device-a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"name":"create_room"`)) {
		t.Fatalf("tools/list response = %d %s", recorder.Code, recorder.Body)
	}
}

func TestFriendPairingRequiresOwnerConfirmation(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/pairing.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, _ := db.EnsureUser("pair-owner")
	requester, _ := db.EnsureUser("pair-requester")
	code, err := db.CreatePairCode(owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(db, nil, "mcp-secret")
	callTool(t, handler, "pair-requester", "accept_friend", map[string]any{"code": code})
	if db.AreFriends(owner, requester) {
		t.Fatal("requester accepting code created friendship before owner confirmation")
	}
	requests, err := db.PendingPairings(owner)
	if err != nil || len(requests) != 1 || requests[0].RequesterID != requester {
		t.Fatalf("pending requests = %+v, %v", requests, err)
	}
	requestID := requests[0].ID
	callTool(t, handler, "pair-owner", "confirm_friendship", map[string]any{"request_id": requestID})
	if !db.AreFriends(owner, requester) {
		t.Fatal("owner confirmation did not establish friendship")
	}
}

func callTool(t *testing.T, handler http.Handler, deviceID, name string, args map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp/message", bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer mcp-secret")
	req.Header.Set("X-Linx-Device-Id", deviceID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tool %s HTTP status = %d: %s", name, rec.Code, rec.Body)
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Result.IsError {
		t.Fatalf("tool %s failed: %s", name, rec.Body)
	}
	if len(response.Result.Content) == 0 {
		return ""
	}
	return response.Result.Content[0].Text
}
