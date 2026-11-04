package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"linx-intercom/server/internal/store"
)

func TestSSEClientInitializesDiscoversCallsAndKeepsDeviceIdentity(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/sse.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	users := map[string]string{}
	for _, id := range []string{"device-a", "device-b"} {
		user, err := db.EnsureUser(id)
		if err != nil {
			t.Fatal(err)
		}
		users[id] = user
	}
	server := httptest.NewServer(NewHandler(db, nil, "sse-secret"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 使用真实 MCP 客户端握手，不能用直接 POST 成功冒充标准 SSE 协议可用。
	for _, id := range []string{"device-a", "device-b"} {
		transport := &linxHeadersTransport{deviceID: id}
		client := sdk.NewClient(&sdk.Implementation{Name: "intercom-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.SSEClientTransport{
			Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: transport},
		}, nil)
		if err != nil {
			t.Fatalf("MCP initialize for %s: %v", id, err)
		}
		defer session.Close()
		listed, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		found := false
		for _, tool := range listed.Tools {
			if tool.Name == "create_room" {
				found = true
			}
		}
		if !found {
			t.Fatal("create_room is absent from tools/list")
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "create_room", Arguments: map[string]any{"name": id + " room", "user_id": "forged-user"}})
		if err != nil || result.IsError {
			t.Fatalf("tools/call for %s: result=%+v error=%v", id, result, err)
		}
		rooms, err := db.RoomsForUser(users[id])
		if err != nil || len(rooms) != 1 || rooms[0].Name != id+" room" {
			t.Fatalf("rooms for %s: %+v, %v", id, rooms, err)
		}
		// 相同 Bearer token 不代表可跨设备复用 SSE session URL。
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, transport.postURL, strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer sse-secret")
		request.Header.Set("X-Linx-Device-Id", "different-device")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-device SSE POST status = %d, want 403", response.StatusCode)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		// 断开 SSE 后旧地址必须失效，不能继续执行迟到的工具请求。
		deadline := time.Now().Add(time.Second)
		for {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, transport.postURL, strings.NewReader(`{"jsonrpc":"2.0","id":100,"method":"ping"}`))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer sse-secret")
			request.Header.Set("X-Linx-Device-Id", id)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode == http.StatusNotFound {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("closed MCP session URL is still registered: status=%d", response.StatusCode)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type linxHeadersTransport struct {
	deviceID string
	once     sync.Once
	postURL  string
}

func (t *linxHeadersTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copyOfRequest := request.Clone(request.Context())
	copyOfRequest.Header.Set("Authorization", "Bearer sse-secret")
	copyOfRequest.Header.Set("X-Linx-Device-Id", t.deviceID)
	if request.Method == http.MethodPost {
		t.once.Do(func() { t.postURL = request.URL.String() })
	}
	return http.DefaultTransport.RoundTrip(copyOfRequest)
}
