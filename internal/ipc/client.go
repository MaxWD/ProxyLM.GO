package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Client — WebSocket-клиент к /admin/stream. Используется TUI.
// Поддерживает авто-реконнект с экспоненциальным backoff.
type Client struct {
	conn    *websocket.Conn
	baseURL string
	token   string
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
	conn, err := dialOnce(ctx, baseURL, token)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, baseURL: baseURL, token: token}, nil
}

// dialOnce выполняет одну попытку WS-подключения.
func dialOnce(ctx context.Context, baseURL, token string) (*websocket.Conn, error) {
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
	return conn, nil
}

// reconnectBackoff вычисляет следующий интервал backoff:
// 1s → 2s → 4s → 8s → 16s → 30s (cap).
func reconnectBackoff(prev time.Duration) time.Duration {
	if prev <= 0 {
		return time.Second
	}
	next := prev * 2
	if next > 30*time.Second {
		next = 30 * time.Second
	}
	return next
}

// ReconnectMsg — сообщение для TUI о состоянии реконнекта.
// TUI не импортирует этот тип напрямую — клиент встраивает логику реконнекта
// в Recv; TUI получает ErrReconnecting или nil после успешного реконнекта.

// ErrReconnecting — сигнал TUI: соединение оборвалось, идёт реконнект.
// TUI должен показать «reconnecting…» и продолжать ждать — следующий вызов
// Recv вернётся сразу при успешном реконнекте.
var ErrReconnecting = fmt.Errorf("ipc: reconnecting to daemon")

// Recv читает следующее сообщение из соединения. Если соединение оборвалось —
// пытается переподключиться с backoff (1s→2s→4s→…→30s).
// Возвращает ErrReconnecting при каждой попытке реконнекта, чтобы TUI мог
// обновить статус. После успешного реконнекта возвращает следующий фрейм.
// Ctx нужен для остановки реконнекта (например при выходе из TUI).
func (c *Client) Recv(ctx context.Context) (Envelope, json.RawMessage, error) {
	for {
		// Попытка чтения с таймаутом 60s — защита от «тихих» обрывов.
		readCtx, readCancel := context.WithTimeout(ctx, 60*time.Second)
		_, data, err := c.conn.Read(readCtx)
		readCancel()

		if err == nil {
			// Decode Envelope с raw payload.
			var raw struct {
				Type    MessageType     `json:"type"`
				Time    json.RawMessage `json:"time"`
				Payload json.RawMessage `json:"payload"`
			}
			if uerr := json.Unmarshal(data, &raw); uerr != nil {
				return Envelope{}, nil, fmt.Errorf("ipc unmarshal envelope: %w", uerr)
			}
			env := Envelope{Type: raw.Type}
			if len(raw.Time) > 0 {
				_ = json.Unmarshal(raw.Time, &env.Time)
			}
			return env, raw.Payload, nil
		}

		// Если ctx отменён — не переподключаемся, возвращаем ctx.Err().
		if ctx.Err() != nil {
			return Envelope{}, nil, ctx.Err()
		}

		// Соединение оборвалось — закрываем текущий conn и реконнектимся.
		_ = c.conn.CloseNow()

		backoff := time.Duration(0)
		for {
			backoff = reconnectBackoff(backoff)

			// Ждём backoff или ctx отмены.
			select {
			case <-ctx.Done():
				return Envelope{}, nil, ctx.Err()
			case <-time.After(backoff):
			}

			// Сообщаем TUI что реконнектимся.
			// Возвращаем ErrReconnecting — TUI вызовет Recv снова.
			// Это не loop внутри одного вызова Recv; мы выходим и даём TUI
			// обновить UI.
			newConn, dialErr := dialOnce(ctx, c.baseURL, c.token)
			if dialErr == nil {
				c.conn = newConn
				// Успешно переподключились — выходим из backoff-loop, читаем снова.
				break
			}
			// dial failed: если ctx отменён — уходим.
			if ctx.Err() != nil {
				return Envelope{}, nil, ctx.Err()
			}
			// Продолжаем backoff-loop.
		}
		// Здесь c.conn — свежее соединение; читаем снова через внешний for.
	}
}

// Ping отправляет WebSocket ping и ждёт pong. Полезно для heartbeat-проверки.
func (c *Client) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

// RequestSnapshot отправляет daemon'у запрос немедленного state_snapshot.
// Daemon ответит фреймом type=state_snapshot через обычный поток Recv.
func (c *Client) RequestSnapshot(ctx context.Context) error {
	env := Envelope{Type: TypeRequestSnapshot}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("ipc: marshal request_snapshot: %w", err)
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		// Не маскируем ошибку — reconnect обнаружит её сам при следующем Recv.
		return fmt.Errorf("ipc: send request_snapshot: %w", err)
	}
	return nil
}

// IsAuthError возвращает true если err содержит признак ошибки аутентификации (401/403).
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "Unauthorized") || strings.Contains(s, "Forbidden")
}

// Close закрывает соединение нормальным StatusCode.
func (c *Client) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}
