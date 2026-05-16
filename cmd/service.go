package cmd

import (
	"context"
	"fmt"

	kardianos "github.com/kardianos/service"
	"github.com/spf13/cobra"

	svcpkg "proxylm/internal/service"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Установка и управление системной службой",
	Long: `Регистрирует proxylm как сервис ОС (Windows Service / systemd / launchd)
через github.com/kardianos/service. Конфиг и БД ищутся рядом с бинарником —
как и при foreground-запуске.`,
}

func buildService(configPath string) (kardianos.Service, error) {
	runner := &svcpkg.Runner{
		DaemonMain: func(ctx context.Context) error {
			return runDaemon(ctx, configPath)
		},
	}
	return svcpkg.Build(svcpkg.Config{ConfigPath: configPath}, runner)
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Зарегистрировать службу в ОС",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("config")
		s, err := buildService(path)
		if err != nil {
			return err
		}
		if err := s.Install(); err != nil {
			return fmt.Errorf("install: %w", err)
		}
		fmt.Println("служба установлена")
		return nil
	},
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Удалить регистрацию службы",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := buildService("")
		if err != nil {
			return err
		}
		if err := s.Uninstall(); err != nil {
			return fmt.Errorf("uninstall: %w", err)
		}
		fmt.Println("служба удалена")
		return nil
	},
}

var serviceStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Запустить службу",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := buildService("")
		if err != nil {
			return err
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		fmt.Println("служба запущена")
		return nil
	},
}

var serviceStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Остановить службу",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := buildService("")
		if err != nil {
			return err
		}
		if err := s.Stop(); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		fmt.Println("служба остановлена")
		return nil
	},
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Показать статус службы",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := buildService("")
		if err != nil {
			return err
		}
		st, err := s.Status()
		if err != nil {
			return fmt.Errorf("status: %w", err)
		}
		switch st {
		case kardianos.StatusRunning:
			fmt.Println("running")
		case kardianos.StatusStopped:
			fmt.Println("stopped")
		default:
			fmt.Println("unknown")
		}
		return nil
	},
}

func init() {
	serviceInstallCmd.Flags().String("config", "", "Путь к config.yaml, который служба будет использовать")
	serviceCmd.AddCommand(serviceInstallCmd, serviceUninstallCmd, serviceStartCmd, serviceStopCmd, serviceStatusCmd)
	rootCmd.AddCommand(serviceCmd)
}
