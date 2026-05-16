package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kardianos/service"
)

// Config описывает регистрацию службы.
type Config struct {
	Name        string
	DisplayName string
	Description string
	// Arguments — аргументы, с которыми будет запускаться бинарник как служба.
	// Обычно: []string{"serve"} — чтобы служба запускала daemon.
	Arguments []string
	// ConfigPath — путь к config.yaml, который служба будет читать. Если пуст —
	// service использует <dir(executable)>/config.yaml (так же как foreground-serve).
	ConfigPath string
}

// Runner — пользовательская реализация service.Interface.
// Start блокирует до завершения daemon'а; Stop отменяет ctx.
type Runner struct {
	// DaemonMain — функция, выполняющая daemon. Получает context.Context;
	// должна вернуться при отмене ctx. Возвращает финальную ошибку (nil — штатно).
	DaemonMain func(ctx context.Context) error

	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan error
}

// Start вызывается kardianos/service в отдельной горутине при старте службы.
func (r *Runner) Start(_ service.Service) error {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.doneCh = make(chan error, 1)
	go func() { r.doneCh <- r.DaemonMain(r.ctx) }()
	return nil
}

// Stop вызывается kardianos/service при остановке службы.
func (r *Runner) Stop(_ service.Service) error {
	if r.cancel != nil {
		r.cancel()
	}
	if r.doneCh != nil {
		// Дождёмся завершения, чтобы оставить чистое состояние.
		<-r.doneCh
	}
	return nil
}

// Build создаёт service.Service для регистрации/управления и для Run().
func Build(cfg Config, runner *Runner) (service.Service, error) {
	if cfg.Name == "" {
		cfg.Name = "proxylm"
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = "ProxyLM.GO"
	}
	if cfg.Description == "" {
		cfg.Description = "OpenAI-compatible proxy for local LLM backends"
	}
	if len(cfg.Arguments) == 0 {
		cfg.Arguments = []string{"serve"}
	}
	if cfg.ConfigPath != "" {
		cfg.Arguments = append(cfg.Arguments, "--config", cfg.ConfigPath)
	}

	// Executable должен быть абсолютным — kardianos записывает его в init-юнит.
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("os.Executable: %w", err)
	}
	exe, _ = filepath.Abs(exe)

	svcCfg := &service.Config{
		Name:             cfg.Name,
		DisplayName:      cfg.DisplayName,
		Description:      cfg.Description,
		Arguments:        cfg.Arguments,
		Executable:       exe,
		WorkingDirectory: filepath.Dir(exe),
	}
	s, err := service.New(runner, svcCfg)
	if err != nil {
		return nil, fmt.Errorf("service.New: %w", err)
	}
	return s, nil
}
