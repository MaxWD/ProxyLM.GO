-- v0.14: персистентность наблюдений PerfTracker (FUTURE #1).
--
-- Каждая строка — одна точка измерения (server, model, endpoint, in/out
-- tokens, totalMs, loaded), идентичная internal/core.perfObservation.
-- id — AUTOINCREMENT и одновременно порядок FIFO: при триме кольцевого
-- буфера на диске удаляются строки с наименьшим id в рамках ключа
-- (server, model, endpoint), т.е. самые старые. created_at — RFC3339Nano UTC,
-- нужен только для форензики (не читается обратно в PerfTracker).

CREATE TABLE perf_observations (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    server     TEXT    NOT NULL,
    model      TEXT    NOT NULL,
    endpoint   TEXT    NOT NULL,
    in_tokens  INTEGER NOT NULL,
    out_tokens INTEGER NOT NULL,
    total_ms   INTEGER NOT NULL,
    loaded     INTEGER NOT NULL,
    created_at TEXT    NOT NULL
);

CREATE INDEX idx_perf_obs_key ON perf_observations(server, model, endpoint, id);
