package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB — обёртка над *sql.DB с настроенным DSN под modernc.org/sqlite.
// Создаётся через Open; перед использованием обязательно вызвать Migrate.
type DB struct {
	*sql.DB
	path string
}

// Open открывает (или создаёт) SQLite-файл по пути path. Путь должен быть
// уже резолвлен (см. config.ResolveRelative). Создаёт родительский каталог,
// если он отсутствует, и применяет PRAGMA, нужные для конкурентной записи.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, fmt.Errorf("storage: пустой path")
	}
	// modernc.org/sqlite принимает file:URI, в котором можно задать PRAGMA через _pragma=.
	dsn := buildDSN(path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// Один writer + ограниченный пул читателей: SQLite обслуживает запись
	// последовательно, поэтому большой пул не даст выигрыша.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(time.Hour)

	ctxPing, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctxPing); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &DB{DB: sqlDB, path: path}, nil
}

// Path возвращает абсолютный путь к файлу БД (для логов и информационных сообщений).
func (d *DB) Path() string { return d.path }

// Migrate применяет все миграции из migrations/*.sql в лексикографическом порядке.
// Каждая миграция применяется внутри транзакции и регистрируется в schema_migrations.
// Идемпотентно: уже применённые миграции пропускаются.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            name        TEXT    PRIMARY KEY,
            applied_at  TEXT    NOT NULL
        )`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		applied, err := d.migrationApplied(ctx, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		content, err := migrationsFS.ReadFile(filepath.ToSlash("migrations/" + name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := d.applyMigration(ctx, name, content); err != nil {
			return err
		}
	}
	return nil
}

// Vacuum выполняет VACUUM — компактирует БД. Используется при storage.vacuum_on_start.
func (d *DB) Vacuum(ctx context.Context) error {
	_, err := d.ExecContext(ctx, "VACUUM")
	if err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

func (d *DB) migrationApplied(ctx context.Context, name string) (bool, error) {
	var n int
	row := d.QueryRowContext(ctx, "SELECT 1 FROM schema_migrations WHERE name = ?", name)
	switch err := row.Scan(&n); err {
	case nil:
		return true, nil
	case sql.ErrNoRows:
		return false, nil
	default:
		return false, fmt.Errorf("check migration %s: %w", name, err)
	}
}

func (d *DB) applyMigration(ctx context.Context, name string, sqlBytes []byte) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO schema_migrations(name, applied_at) VALUES (?, ?)",
		name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

// buildDSN формирует file:URI с PRAGMA, нужными для записи под конкурентной нагрузкой.
// modernc.org/sqlite поддерживает параметр _pragma= с repeat значениями.
func buildDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	// file:<path>?... — путь форматируем как есть; для Windows '\' допустим.
	return "file:" + path + "?" + q.Encode()
}
