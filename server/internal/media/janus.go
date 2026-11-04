package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type janusEnvelope map[string]json.RawMessage

type janusClient struct {
	conn       *websocket.Conn
	apiSecret  string
	readCancel context.CancelFunc
	writeMu    sync.Mutex
	mu         sync.Mutex
	pending    map[string]chan janusEnvelope
	done       chan struct{}
}

func connectJanus(ctx context.Context, url, apiSecret string) (*janusClient, uint64, error) {
	if url == "" {
		return nil, 0, errors.New("Janus websocket URL is required")
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("connect Janus websocket: %w", err)
	}
	readCtx, readCancel := context.WithCancel(context.Background())
	client := &janusClient{conn: conn, apiSecret: apiSecret, readCancel: readCancel, pending: make(map[string]chan janusEnvelope), done: make(chan struct{})}
	go client.readLoop(readCtx)
	var created struct {
		Data struct {
			ID uint64 `json:"id"`
		} `json:"data"`
	}
	request := map[string]any{"janus": "create", "transaction": transactionID()}
	if apiSecret != "" {
		request["apisecret"] = apiSecret
	}
	if err := client.exchange(ctx, request, &created, nil); err != nil {
		client.close()
		return nil, 0, err
	}
	if created.Data.ID == 0 {
		client.close()
		return nil, 0, errors.New("Janus create returned an empty session ID")
	}
	return client, created.Data.ID, nil
}

// EnsureRoom makes dynamic AudioBridge creation idempotent across Go restarts.
func EnsureRoom(ctx context.Context, config Config, roomID uint64, description string) error {
	if roomID == 0 {
		return errors.New("Janus room ID is required")
	}
	client, sessionID, err := connectJanus(ctx, config.WebSocketURL, config.APISecret)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.exchange(closeCtx, map[string]any{"janus": "destroy", "session_id": sessionID}, nil, nil); err != nil {
			slog.Warn("Janus room provisioning session destroy failed", "janus_session_id", sessionID, "error", err)
		}
		client.close()
	}()
	var attached struct {
		Data struct {
			ID uint64 `json:"id"`
		} `json:"data"`
	}
	if err := client.exchange(ctx, map[string]any{"janus": "attach", "session_id": sessionID, "plugin": "janus.plugin.audiobridge"}, &attached, nil); err != nil {
		return fmt.Errorf("attach AudioBridge for room provisioning: %w", err)
	}
	if attached.Data.ID == 0 {
		return errors.New("Janus room provisioning returned an empty handle ID")
	}
	request := func(body map[string]any) (janusEnvelope, error) {
		var response janusEnvelope
		err := client.exchange(ctx, map[string]any{"janus": "message", "session_id": sessionID, "handle_id": attached.Data.ID, "body": body}, &response, nil)
		return response, err
	}
	response, err := request(map[string]any{"request": "exists", "room": roomID})
	if err != nil {
		return fmt.Errorf("check Janus AudioBridge room: %w", err)
	}
	var exists struct {
		Data struct {
			Exists bool `json:"exists"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response["plugindata"], &exists); err != nil {
		return fmt.Errorf("decode Janus room existence response: %w", err)
	}
	if exists.Data.Exists {
		slog.Info("Janus AudioBridge room already exists", "room_id", roomID)
		return nil
	}
	body := map[string]any{"request": "create", "room": roomID, "description": description, "sampling_rate": 8000}
	if config.AdminKey != "" {
		body["admin_key"] = config.AdminKey
	}
	response, err = request(body)
	if err != nil {
		return fmt.Errorf("create Janus AudioBridge room: %w", err)
	}
	var created struct {
		Data struct {
			Status string `json:"audiobridge"`
			Room   uint64 `json:"room"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response["plugindata"], &created); err != nil {
		return fmt.Errorf("decode Janus room create response: %w", err)
	}
	if created.Data.Status != "created" || created.Data.Room != roomID {
		return fmt.Errorf("Janus room create returned unexpected response for room %d", roomID)
	}
	slog.Info("Janus AudioBridge room created", "room_id", roomID)
	return nil
}

func DestroyRoom(ctx context.Context, config Config, roomID uint64) error {
	if roomID == 0 {
		return errors.New("Janus room ID is required")
	}
	client, sessionID, err := connectJanus(ctx, config.WebSocketURL, config.APISecret)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.exchange(closeCtx, map[string]any{"janus": "destroy", "session_id": sessionID}, nil, nil); err != nil {
			slog.Warn("Janus room destroy session cleanup failed", "janus_session_id", sessionID, "error", err)
		}
		client.close()
	}()
	var attached struct {
		Data struct {
			ID uint64 `json:"id"`
		} `json:"data"`
	}
	if err := client.exchange(ctx, map[string]any{"janus": "attach", "session_id": sessionID, "plugin": "janus.plugin.audiobridge"}, &attached, nil); err != nil {
		return fmt.Errorf("attach AudioBridge for room destruction: %w", err)
	}
	if attached.Data.ID == 0 {
		return errors.New("Janus room destruction returned an empty handle ID")
	}
	var response janusEnvelope
	err = client.exchange(ctx, map[string]any{
		"janus": "message", "session_id": sessionID, "handle_id": attached.Data.ID,
		"body": map[string]any{"request": "destroy", "room": roomID, "permanent": false},
	}, &response, nil)
	if err != nil {
		return fmt.Errorf("destroy Janus AudioBridge room: %w", err)
	}
	slog.Info("Janus AudioBridge room destroy requested", "room_id", roomID)
	return nil
}

func (c *janusClient) exchange(ctx context.Context, request map[string]any, result any, terminal func(janusEnvelope) bool) error {
	transaction := transactionID()
	request["transaction"] = transaction
	if c.apiSecret != "" {
		request["apisecret"] = c.apiSecret
	}
	responses := make(chan janusEnvelope, 8)
	c.mu.Lock()
	c.pending[transaction] = responses
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, transaction)
		c.mu.Unlock()
	}()
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	err = c.conn.Write(ctx, websocket.MessageText, data)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("send Janus request: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("Janus signaling connection closed")
		case response := <-responses:
			if terminal != nil && !terminal(response) {
				continue
			}
			var status string
			_ = json.Unmarshal(response["janus"], &status)
			if status == "error" {
				return fmt.Errorf("Janus returned error: %s", response["error"])
			}
			if result != nil {
				if err := json.Unmarshal(rawEnvelope(response), result); err != nil {
					return fmt.Errorf("decode Janus response: %w", err)
				}
			}
			return nil
		}
	}
}

func (c *janusClient) close() {
	c.readCancel()
	_ = c.conn.CloseNow()
}

func (c *janusClient) readLoop(ctx context.Context) {
	defer close(c.done)
	for {
		_, payload, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("Janus signaling read failed", "error", err)
			}
			return
		}
		var message janusEnvelope
		if err := json.Unmarshal(payload, &message); err != nil {
			slog.Warn("Janus signaling message rejected", "error", err)
			continue
		}
		var transaction string
		_ = json.Unmarshal(message["transaction"], &transaction)
		c.mu.Lock()
		responses := c.pending[transaction]
		c.mu.Unlock()
		if responses != nil {
			select {
			case responses <- message:
			default:
				slog.Warn("Janus response queue full", "transaction", transaction)
			}
		}
	}
}

func rawEnvelope(envelope janusEnvelope) []byte {
	data, _ := json.Marshal(envelope)
	return data
}

func transactionID() string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
