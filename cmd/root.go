package cmd

import (
	"github.com/spf13/cobra"
)

var (
	// version устанавливается из main через SetVersion (тот заполняется -ldflags).
	versionString = "dev"

	// defaultConfigTemplate — содержимое config.example.yaml, встроенное в бинарник.
	// Передаётся из main через SetDefaultConfigTemplate.
	defaultConfigTemplate []byte
)

var rootCmd = &cobra.Command{
	Use:   "proxylm",
	Short: "OpenAI-совместимый прокси перед локальными LLM с model-aware queueing и TUI",
	Long: `proxylm — single-binary прокси на Go.

Запуск:
  proxylm serve                                                     — daemon
  proxylm tui --connect ws://localhost:8080 --token <admin_key>     — TUI-клиент
  proxylm config init|validate                                      — работа с конфигом
  proxylm service install|uninstall|start|stop|status               — управление службой

При первом запуске serve рядом с бинарником создаются config.yaml и proxylm.db.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute — точка входа, вызывается из main.
func Execute() error {
	return rootCmd.Execute()
}

// SetVersion вызывается main.go с версией, прошитой через -ldflags.
func SetVersion(v string) {
	versionString = v
	rootCmd.Version = v
	SetDaemonVersion(v)
}

// SetDefaultConfigTemplate передаёт содержимое embedded config.example.yaml.
// Используется командой serve для автогенерации config.yaml при первом запуске
// и командой config init.
func SetDefaultConfigTemplate(b []byte) {
	defaultConfigTemplate = b
}
