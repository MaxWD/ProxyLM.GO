package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"proxylm/internal/webui"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Запустить локальный Web UI (браузерный аналог tui)",
	Long: `Поднимает локальный HTTP-сервер со встроенным read-only дашбордом и
открывает его в браузере. Данные идут напрямую из daemon'а по WebSocket
(/admin/stream) — так же, как в tui. Сам daemon статику Web UI не отдаёт.

Пример: proxylm web --connect ws://localhost:8080
        proxylm web --connect ws://localhost:8080 --token sk-admin-... --listen 127.0.0.1:9090`,
	RunE: func(cmd *cobra.Command, args []string) error {
		connect, _ := cmd.Flags().GetString("connect")
		token, _ := cmd.Flags().GetString("token")
		listen, _ := cmd.Flags().GetString("listen")
		noOpen, _ := cmd.Flags().GetBool("no-open")

		if connect == "" {
			return errors.New("--connect обязателен (например ws://localhost:8080)")
		}
		wsURL, err := adminStreamURL(connect)
		if err != nil {
			return err
		}

		mux := http.NewServeMux()
		mux.HandleFunc("/config.js", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(webui.ConfigJS(wsURL, token))
		})
		mux.Handle("/", webui.Handler())

		ln, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("listen %s: %w", listen, err)
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

		pageURL := "http://" + ln.Addr().String() + "/"
		fmt.Printf("Web UI: %s (daemon: %s)\nCtrl+C — остановить.\n", pageURL, wsURL)
		if !noOpen {
			openBrowser(pageURL) // best-effort; при неудаче URL уже напечатан
		}

		errCh := make(chan error, 1)
		go func() { errCh <- srv.Serve(ln) }()
		select {
		case <-cmd.Context().Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	},
}

// adminStreamURL нормализует --connect до полного WebSocket-URL /admin/stream.
// Принимает ws:// и wss:// (как у tui), а также http:// и https:// (схема
// конвертируется). Уже указанный путь /admin/stream не дублируется.
func adminStreamURL(connect string) (string, error) {
	u := strings.TrimRight(connect, "/")
	switch {
	case strings.HasPrefix(u, "ws://"), strings.HasPrefix(u, "wss://"):
		// как есть
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	default:
		return "", fmt.Errorf("--connect: неподдерживаемая схема в %q (нужен ws:// или http://)", connect)
	}
	if !strings.HasSuffix(u, "/admin/stream") {
		u += "/admin/stream"
	}
	return u, nil
}

// openBrowser открывает URL в браузере по умолчанию. Best-effort: ошибки
// игнорируются — URL уже напечатан в stdout, пользователь может открыть руками.
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}

func init() {
	webCmd.Flags().String("connect", "ws://localhost:8080", "WebSocket URL daemon'а (как у tui)")
	webCmd.Flags().String("token", "", "admin_key: если задан — страница подключается сразу, без формы")
	webCmd.Flags().String("listen", "127.0.0.1:8081", "адрес локального HTTP-сервера Web UI")
	webCmd.Flags().Bool("no-open", false, "не открывать браузер автоматически")
	webCmd.SilenceUsage = true
	rootCmd.AddCommand(webCmd)
}
