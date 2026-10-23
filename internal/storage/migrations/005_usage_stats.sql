-- v0.15: статистика использования моделей. run_ms — суммарное время
-- выполнения на бэкендах по всем попыткам, queue_ms — ожидание в очереди до
-- первого dispatch. NULL означает «не измерено» (строки до v0.15 или запрос,
-- не дошедший до бэкенда); агрегатор UsageStats для таких строк делает
-- fallback на completed_at - started_at.
--
-- Индекс (created_at, model) ускоряет выборки по окну времени.

ALTER TABLE requests ADD COLUMN run_ms INTEGER;
ALTER TABLE requests ADD COLUMN queue_ms INTEGER;

CREATE INDEX IF NOT EXISTS idx_requests_created_model ON requests(created_at, model);
