package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// Client — WebSocket-клиент к /admin/stream. Используется TUI.
type Client struct {
	conn *websocket.Conn
}

// clientReadLimit — потолок размера одного входящего WS-фрейма от daemon'а.
// Дефолт coder/websocket — 32 KiB; полный state_snapshot на 100 запросов + N серверов
// легко превышает 32 KiB и приводит к разрыву соединения с "message too big".
// 4 MiB — щедрый запас для любого реалистичного snapshot'а; источник доверенный (свой daemon).
const clientReadLimit = 4 * 1024 * 1024

// Dial подключается к u (например ws://localhost:8080) и аутентифицируется через token.
// Endpoint фиксирован: /admin/stream.
func Dial(ctx context.Context, baseURL, token string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ipc: пустой baseURL")
	}
	if token == "" {
		return nil, fmt.Errorf("ipc: пустой token")
	}
	url := baseURL + "/admin/stream"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + token},
		},
	})
	// On failed handshake coder/websocket returns the *http.Response so the
	// caller can read the body for error details; we don't, so just drain.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("ipc dial %s: %w", url, err)
	}
	conn.SetReadLimit(clientReadLimit)
	return &Client{conn: conn}, nil
}

// Recv читает следующее сообщение из соединения. Возвращает распарсенный Envelope
// с raw payload в виде json.RawMessage — TUI/тесты сами знают, как распарсить дальше.
func (c *Client) Recv(ctx context.Context) (Envelope, json.RawMessage, error) {
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		return Envelope{}, nil, err
	}
	// Decode Envelope с raw payload.
	var raw struct {
		Type    MessageType     `json:"type"`
		Time    json.RawMessage `json:"time"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Envelope{}, nil, fmt.Errorf("ipc unmarshal envelope: %w", err)
	}
	env := Envelope{Type: raw.Type}
	if len(raw.Time) > 0 {
		_ = json.Unmarshal(raw.Time, &env.Time)
	}
	return env, raw.Payload, nil
}

// Close закрывает соединение нормальным StatusCode.
func (c *Client) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}
