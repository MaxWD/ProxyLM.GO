package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"proxylm/internal/config"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Работа с конфигом (init, validate)",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Создать config.yaml из встроенного шаблона (если ещё не существует)",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("path")
		force, _ := cmd.Flags().GetBool("force")
		if path == "" {
			path = config.DefaultConfigPath()
		}

		if _, err := os.Stat(path); err == nil && !force {
			return fmt.Errorf("файл уже существует: %s (используйте --force чтобы перезаписать)", path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, defaultConfigTemplate, 0o644); err != nil {
			return err
		}
		fmt.Printf("создан: %s\n", path)
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Проверить config.yaml на корректность",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("path")
		if path == "" {
			path = config.DefaultConfigPath()
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("config %s невалиден: %w", path, err)
		}
		fmt.Printf("OK: %s (backends=%d, api_keys=%d, strategy=%s)\n",
			path, len(cfg.Backends), len(cfg.Auth.APIKeys), cfg.Routing.Strategy)
		return nil
	},
}

func init() {
	configInitCmd.Flags().String("path", "", "Путь к config.yaml (по умолчанию — рядом с бинарником)")
	configInitCmd.Flags().Bool("force", false, "Перезаписать существующий файл")
	configValidateCmd.Flags().String("path", "", "Путь к config.yaml (по умолчанию — рядом с бинарником)")
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configValidateCmd)
	rootCmd.AddCommand(configCmd)
}
