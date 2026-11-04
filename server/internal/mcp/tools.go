package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"linx-intercom/server/internal/store"
)

type OnlineFunc func(userID string) bool

type Handler struct {
	store    *store.Store
	online   OnlineFunc
	token    string
	rooms    RoomHooks
	streamMu sync.Mutex
	streams  map[string]*sseSession
}

type RoomHooks struct {
	Join    func(context.Context, string, store.Room) error
	Destroy func(context.Context, store.Room) error
}

func NewHandler(db *store.Store, online OnlineFunc, token string, roomHooks ...RoomHooks) http.Handler {
	h := &Handler{store: db, online: online, token: token, streams: make(map[string]*sseSession)}
	if len(roomHooks) > 0 {
		h.rooms = roomHooks[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mcp", h.sse)
	mux.HandleFunc("POST /mcp/message", h.message)
	return mux
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	} `json:"params"`
}

type rpcResponse struct {
	JSONRPC string     `json:"jsonrpc"`
	ID      any        `json:"id"`
	Result  toolResult `json:"result"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content,omitempty"`
	IsError bool       `json:"isError,omitempty"`
	Tools   []toolSpec `json:"tools,omitempty"`
}

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (h *Handler) message(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Has("sessionid") {
		h.sseMessage(w, r)
		return
	}
	deviceID := r.Header.Get("X-Linx-Device-Id")
	userID, err := h.store.UserForDevice(deviceID)
	if err != nil {
		http.Error(w, "unknown device", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var request rpcRequest
	if err := json.Unmarshal(body, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	result := h.execute(r.Context(), request, deviceID, userID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage(request.ID), Result: result})
}

func (h *Handler) execute(ctx context.Context, request rpcRequest, deviceID, userID string) toolResult {
	if request.Method == "tools/call" {
		// 不记录 arguments：参数中可能有一次性配对码等不适合进入日志的内容。
		slog.Info("MCP tool call started", "device_id", deviceID, "user_id", userID, "tool", request.Params.Name)
	}
	result, err := h.call(ctx, request, userID)
	if err != nil {
		slog.Warn("MCP tool call failed", "device_id", deviceID, "tool", request.Params.Name, "error", err)
		result.IsError = true
		result.Content = textContent("操作未完成：" + err.Error()).Content
	} else if request.Method == "tools/call" {
		slog.Info("MCP tool call completed", "device_id", deviceID, "user_id", userID, "tool", request.Params.Name)
	}
	return result
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.token == "" {
		return false
	}
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	token := header[len(prefix):]
	return len(token) == len(h.token) && subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

func (h *Handler) call(ctx context.Context, request rpcRequest, userID string) (toolResult, error) {
	if request.Method == "tools/list" {
		return toolResult{Tools: []toolSpec{
			{Name: "create_room", Description: "创建语音对讲房间", InputSchema: objectSchema(map[string]any{"name": stringSchema("房间名称")})},
			{Name: "pair_friend", Description: "生成一次性好友配对码", InputSchema: objectSchema(nil)},
			{Name: "accept_friend", Description: "提交面对面交换的一次性好友配对码", InputSchema: objectSchema(map[string]any{"code": stringSchema("对方设备读出的配对码")})},
			{Name: "list_pairing_requests", Description: "查询等待本人确认的好友配对请求", InputSchema: objectSchema(nil)},
			{Name: "confirm_friendship", Description: "确认好友配对请求", InputSchema: objectSchema(map[string]any{"request_id": stringSchema("待确认配对请求编号")})},
			{Name: "invite_member", Description: "邀请在线好友进入房间", InputSchema: objectSchema(map[string]any{"room_id": stringSchema("房间编号"), "device_id": stringSchema("好友设备编号")})},
			{Name: "list_pending_invitations", Description: "查询等待本人确认的房间邀请", InputSchema: objectSchema(nil)},
			{Name: "accept_invitation", Description: "确认加入房间邀请", InputSchema: objectSchema(map[string]any{"invitation_id": stringSchema("邀请编号")})},
			{Name: "list_online_members", Description: "查询房间当前在线成员", InputSchema: objectSchema(map[string]any{"room_id": stringSchema("房间编号")})},
		}}, nil
	}
	if request.Method != "tools/call" {
		return toolResult{}, errors.New("unsupported MCP method")
	}
	args := request.Params.Arguments
	switch request.Params.Name {
	case "create_room":
		name, err := stringArg(args, "name")
		if err != nil {
			return toolResult{}, err
		}
		roomID, err := h.store.CreateRoom(userID, name)
		if err != nil {
			return toolResult{}, err
		}
		room, err := h.store.GetRoom(roomID)
		if err != nil {
			return toolResult{}, err
		}
		if h.rooms.Join != nil {
			if err := h.rooms.Join(ctx, userID, room); err != nil {
				failure := fmt.Errorf("create or join audio room: %w", err)
				if h.rooms.Destroy != nil {
					if destroyErr := h.rooms.Destroy(ctx, room); destroyErr != nil {
						slog.Error("failed to clean up Janus room after create failure", "room_id", room.ID, "error", destroyErr)
						failure = fmt.Errorf("%w (Janus cleanup failed: %v)", failure, destroyErr)
					}
				}
				if deleteErr := h.store.DeleteRoom(room.ID); deleteErr != nil {
					slog.Error("failed to roll back room creation", "room_id", room.ID, "error", deleteErr)
					failure = fmt.Errorf("%w (database rollback failed: %v)", failure, deleteErr)
				}
				return toolResult{}, failure
			}
		}
		slog.Info("intercom room created", "room_id", room.ID, "janus_room_id", room.JanusID, "owner_id", userID)
		return textContent(fmt.Sprintf("房间已创建，编号 %s", roomID)), nil
	case "pair_friend":
		code, err := h.store.CreatePairCode(userID, 5*time.Minute)
		if err != nil {
			return toolResult{}, err
		}
		return textContent(fmt.Sprintf("一次性配对码：%s，有效期 5 分钟", code)), nil
	case "accept_friend":
		code, err := stringArg(args, "code")
		if err != nil {
			return toolResult{}, err
		}
		if _, err := h.store.AcceptPairCode(userID, code); err != nil {
			return toolResult{}, err
		}
		return textContent("配对请求已提交；发码方还需查询并语音确认后才会成为好友"), nil
	case "list_pairing_requests":
		requests, err := h.store.PendingPairings(userID)
		if err != nil {
			return toolResult{}, err
		}
		payload, _ := json.Marshal(requests)
		return textContent(string(payload)), nil
	case "confirm_friendship":
		requestID, err := stringArg(args, "request_id")
		if err != nil {
			return toolResult{}, err
		}
		if err := h.store.ConfirmFriendship(userID, requestID); err != nil {
			return toolResult{}, err
		}
		return textContent("好友双方已确认，配对成功"), nil
	case "invite_member":
		roomID, err := stringArg(args, "room_id")
		if err != nil {
			return toolResult{}, err
		}
		targetDeviceID, err := stringArg(args, "device_id")
		if err != nil {
			return toolResult{}, err
		}
		targetUser, err := h.store.UserForDevice(targetDeviceID)
		if err != nil {
			return toolResult{}, errors.New("好友设备不存在")
		}
		invitationID, err := h.store.Invite(roomID, userID, targetUser, h.online != nil && h.online(targetUser))
		if err != nil {
			return toolResult{}, err
		}
		return textContent(fmt.Sprintf("邀请已发送，等待对方确认，编号 %s", invitationID)), nil
	case "list_pending_invitations":
		invitations, err := h.store.PendingInvitations(userID)
		if err != nil {
			return toolResult{}, err
		}
		payload, _ := json.Marshal(invitations)
		return textContent(string(payload)), nil
	case "accept_invitation":
		invitationID, err := stringArg(args, "invitation_id")
		if err != nil {
			return toolResult{}, err
		}
		room, err := h.store.InvitationRoom(invitationID, userID)
		if err != nil {
			return toolResult{}, err
		}
		if err := h.store.AcceptInvitation(invitationID, userID); err != nil {
			return toolResult{}, err
		}
		if h.rooms.Join != nil {
			if err := h.rooms.Join(ctx, userID, room); err != nil {
				failure := fmt.Errorf("join invited audio room: %w", err)
				if rollbackErr := h.store.RollbackInvitationAcceptance(invitationID, userID); rollbackErr != nil {
					slog.Error("failed to roll back room invitation acceptance", "invitation_id", invitationID, "room_id", room.ID, "error", rollbackErr)
					failure = fmt.Errorf("%w (invitation rollback failed: %v)", failure, rollbackErr)
				}
				return toolResult{}, failure
			}
		}
		slog.Info("device accepted room invitation", "room_id", room.ID, "user_id", userID)
		return textContent("已加入房间"), nil
	case "list_online_members":
		roomID, err := stringArg(args, "room_id")
		if err != nil {
			return toolResult{}, err
		}
		members, err := h.store.RoomMembers(roomID)
		if err != nil {
			return toolResult{}, err
		}
		var online []string
		for _, member := range members {
			if h.online != nil && h.online(member) {
				online = append(online, member)
			}
		}
		payload, _ := json.Marshal(online)
		return textContent(string(payload)), nil
	default:
		return toolResult{}, errors.New("unknown tool")
	}
}

func objectSchema(properties map[string]any) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	return map[string]any{"type": "object", "properties": properties, "required": requiredKeys(properties), "additionalProperties": false}
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description, "minLength": 1}
}

func requiredKeys(properties map[string]any) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	return keys
}

func stringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("missing string argument %q", key)
	}
	return value, nil
}

func textContent(text string) toolResult {
	return toolResult{Content: []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{{Type: "text", Text: text}}}
}
