-- 0003_series: 剧场（短剧）。剧集只引用已有 media，不复制文件、不上传。
-- position 是 1 起的顺序，同时就是界面上的「第 N 集」；唯一索引保证不重复。
CREATE TABLE IF NOT EXISTS series (
  id             TEXT PRIMARY KEY,
  title          TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  cover_media_id TEXT,
  library_id     TEXT,
  sort_order     INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  deleted_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_series_order ON series(sort_order, created_at);

CREATE TABLE IF NOT EXISTS series_media (
  series_id  TEXT NOT NULL REFERENCES series(id) ON DELETE CASCADE,
  media_id   TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  position   INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (series_id, media_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_series_media_position ON series_media(series_id, position);
CREATE INDEX IF NOT EXISTS idx_series_media_media ON series_media(media_id);
