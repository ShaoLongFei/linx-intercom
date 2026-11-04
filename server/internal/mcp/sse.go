package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type sseSession struct {
	deviceID  string
	transport *sdk.SSEServerTransport
}

func (h *Handler) sse(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	deviceID := r.Header.Get("X-Linx-Device-Id")
	userID, err := h.store.UserForDevice(deviceID)
	if err != nil {
		http.Error(w, "unknown device", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	streamID := rand.Text()
	transport := &sdk.SSEServerTransport{
		// 相对路径沿用客户端的 HTTPS origin，避免 TLS 代理后错误地通告明文 HTTP。
		Endpoint:            "/mcp/message?sessionid=" + streamID,
		Response:            w,
		MaxRequestBodyBytes: 1 << 20,
	}
	server := h.protocolServer(deviceID, userID)
	// Connect 会先发送 endpoint 事件；互斥锁保证并发 POST 看不到未连接完成的 transport。
	h.streamMu.Lock()
	session, err := server.Connect(r.Context(), transport, nil)
	if err == nil {
		h.streams[streamID] = &sseSession{deviceID: deviceID, transport: transport}
	}
	h.streamMu.Unlock()
	if err != nil {
		slog.Warn("MCP SSE connect failed", "device_id", deviceID, "error", err)
		return
	}
	defer func() {
		h.streamMu.Lock()
		delete(h.streams, streamID)
		h.streamMu.Unlock()
		_ = session.Close()
		slog.Info("MCP SSE disconnected", "device_id", deviceID)
	}()
	slog.Info("MCP SSE connected", "device_id", deviceID, "user_id", userID)
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-r.Context().Done():
	case err := <-done:
		if err != nil {
			slog.Warn("MCP SSE protocol stopped", "device_id", deviceID, "error", err)
		}
	}
}

func (h *Handler) sseMessage(w http.ResponseWriter, r *http.Request) {
	h.streamMu.Lock()
	session := h.streams[r.URL.Query().Get("sessionid")]
	h.streamMu.Unlock()
	if session == nil {
		http.Error(w, "MCP session not found", http.StatusNotFound)
		return
	}
	if session.deviceID != r.Header.Get("X-Linx-Device-Id") {
		slog.Warn("MCP SSE identity mismatch rejected", "device_id", r.Header.Get("X-Linx-Device-Id"))
		http.Error(w, "MCP device identity mismatch", http.StatusForbidden)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	// SDK 负责 JSON-RPC initialize/notifications/ping 和通过 SSE 返回响应；POST 仅返回 202。
	session.transport.ServeHTTP(w, r)
}

func (h *Handler) protocolServer(deviceID, userID string) *sdk.Server {
	server := sdk.NewServer(&sdk.Implementation{Name: "linx-intercom", Version: "1"}, nil)
	listed, _ := h.call(context.Background(), rpcRequest{Method: "tools/list"}, userID)
	for _, tool := range listed.Tools {
		server.AddTool(&sdk.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema},
			func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				request := rpcRequest{Method: "tools/call"}
				request.Params.Name = req.Params.Name
				if len(req.Params.Arguments) != 0 {
					if err := json.Unmarshal(req.Params.Arguments, &request.Params.Arguments); err != nil {
						return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "工具参数必须是 JSON 对象"}}}, nil
					}
				}
				// 身份取自建连时认证的设备头，不接受模型 arguments 中的用户或设备身份。
				result := h.execute(ctx, request, deviceID, userID)
				response := &sdk.CallToolResult{IsError: result.IsError}
				for _, content := range result.Content {
					response.Content = append(response.Content, &sdk.TextContent{Text: content.Text})
				}
				return response, nil
			})
	}
	return server
}
