-- 0002_feed: 每用户随机播放游标 + 播放设置。
-- scope 为空串代表"全部库"，否则是 library_id：切库各有独立的一轮。
CREATE TABLE IF NOT EXISTS feed_state (
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  scope       TEXT NOT NULL DEFAULT '',
  seed        TEXT NOT NULL,
  cursor_hash INTEGER NOT NULL DEFAULT 0,
  cursor_id   TEXT NOT NULL DEFAULT '',
  played      INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT NOT NULL,
  PRIMARY KEY (user_id, scope)
);

CREATE TABLE IF NOT EXISTS user_prefs (
  user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  loop_play     INTEGER NOT NULL DEFAULT 0,
  autoplay_next INTEGER NOT NULL DEFAULT 1,
  updated_at    TEXT NOT NULL
);
