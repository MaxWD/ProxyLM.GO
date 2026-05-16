-- Схема таблицы requests — единая запись о каждом обработанном запросе.
-- Соответствует core.RequestRecord. Все timestamp хранятся как ISO-8601 UTC
-- (TEXT) для портабельности и удобной сериализации через JSON в IPC.

CREATE TABLE IF NOT EXISTS requests (
    id              TEXT    PRIMARY KEY,
    client_name     TEXT    NOT NULL,
    model           TEXT    NOT NULL,
    endpoint        TEXT    NOT NULL,
    stream          INTEGER NOT NULL DEFAULT 0,
    server_name     TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL,
    http_status     INTEGER NOT NULL DEFAULT 0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    attempt         INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL,
    started_at      TEXT    NOT NULL DEFAULT '',
    completed_at    TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests (created_at);
CREATE INDEX IF NOT EXISTS idx_requests_status     ON requests (status);
CREATE INDEX IF NOT EXISTS idx_requests_model      ON requests (model);
CREATE INDEX IF NOT EXISTS idx_requests_client     ON requests (client_name);

-- Таблица для отслеживания применённых миграций (по имени файла).
CREATE TABLE IF NOT EXISTS schema_migrations (
    name        TEXT    PRIMARY KEY,
    applied_at  TEXT    NOT NULL
);
