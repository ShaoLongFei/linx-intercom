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

	"github.com/coder/websocket"
)

type janusEnvelope map[string]json.RawMessage

type janusClient struct {
	conn      *websocket.Conn
	apiSecret string
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan janusEnvelope
	done      chan struct{}
}

func connectJanus(ctx context.Context, url, apiSecret string) (*janusClient, uint64, error) {
	if url == "" {
		return nil, 0, errors.New("Janus websocket URL is required")
	}
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("connect Janus websocket: %w", err)
	}
	client := &janusClient{conn: conn, apiSecret: apiSecret, pending: make(map[string]chan janusEnvelope), done: make(chan struct{})}
	go client.readLoop(ctx)
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
		_ = conn.CloseNow()
		return nil, 0, err
	}
	if created.Data.ID == 0 {
		_ = conn.CloseNow()
		return nil, 0, errors.New("Janus create returned an empty session ID")
	}
	return client, created.Data.ID, nil
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
