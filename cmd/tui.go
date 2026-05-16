package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"proxylm/internal/ipc"
	"proxylm/internal/tui"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Запустить TUI-клиент к daemon'у",
	Long: `Подключается к запущенному daemon'у по WebSocket (/admin/stream)
и показывает live-состояние очередей, in-flight запросов и логов.

Пример: proxylm tui --connect ws://localhost:8080 --token sk-admin-...`,
	RunE: func(cmd *cobra.Command, args []string) error {
		connect, _ := cmd.Flags().GetString("connect")
		token, _ := cmd.Flags().GetString("token")
		if connect == "" {
			return errors.New("--connect обязателен (например ws://localhost:8080)")
		}
		if token == "" {
			return errors.New("--token обязателен (admin_key из config.yaml)")
		}
		client, err := ipc.Dial(cmd.Context(), connect, token)
		if err != nil {
			return fmt.Errorf("connect %s: %w", connect, err)
		}
		defer client.Close()
		return tui.Run(cmd.Context(), client)
	},
}

func init() {
	tuiCmd.Flags().String("connect", "ws://localhost:8080", "WebSocket URL daemon'а")
	tuiCmd.Flags().String("token", "", "admin_key для аутентификации")
	// При ошибке IPC хотим увидеть конкретный текст, а не помощь по команде.
	tuiCmd.SilenceUsage = true
	rootCmd.AddCommand(tuiCmd)
}
