-- 0009_autoscan: 自动扫描（定时增量 + 事件加速器）的设置与运行记录。
-- 设置单行（id=1）；运行记录追加写，如实语义：跳过/失败必须带原因（note）。
CREATE TABLE IF NOT EXISTS autoscan_settings (
  id               INTEGER PRIMARY KEY CHECK (id = 1),
  enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0,1)),
  interval_minutes INTEGER NOT NULL DEFAULT 5,
  events_enabled   INTEGER NOT NULL DEFAULT 1 CHECK (events_enabled IN (0,1)),
  debounce_seconds INTEGER NOT NULL DEFAULT 8,
  updated_at       TEXT NOT NULL
);
INSERT OR IGNORE INTO autoscan_settings
  (id, enabled, interval_minutes, events_enabled, debounce_seconds, updated_at)
  VALUES (1, 1, 5, 1, 8, '1970-01-01T00:00:00Z');

CREATE TABLE IF NOT EXISTS autoscan_runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  trigger     TEXT NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('success','partial','skipped','failed')),
  started_at  TEXT NOT NULL,
  finished_at TEXT NOT NULL DEFAULT '',
  libraries   INTEGER NOT NULL DEFAULT 0,
  started     INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  updated     INTEGER NOT NULL DEFAULT 0,
  new_media   INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  missing     INTEGER NOT NULL DEFAULT 0,
  events_ok   INTEGER NOT NULL DEFAULT 1 CHECK (events_ok IN (0,1)),
  events_note TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_autoscan_runs_started ON autoscan_runs(started_at DESC, id DESC);
