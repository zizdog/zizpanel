-- 0001_init: MVP schema. All timestamps are RFC3339 UTC text.
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  username      TEXT NOT NULL,
  display_name  TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('admin','user')),
  status        TEXT NOT NULL CHECK (status IN ('active','disabled')),
  failed_attempts INTEGER NOT NULL DEFAULT 0,
  locked_until  TEXT,
  last_login_at TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  deleted_at    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_username ON users(username) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS media_libraries (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  root_path    TEXT NOT NULL,
  recursive    INTEGER NOT NULL DEFAULT 1,
  enabled      INTEGER NOT NULL DEFAULT 1,
  ignore_rules TEXT NOT NULL DEFAULT '[]',
  mount_id     TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  deleted_at   TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_libraries_name ON media_libraries(name) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS media (
  id              TEXT PRIMARY KEY,
  library_id      TEXT NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
  path            TEXT NOT NULL,
  normalized_path TEXT NOT NULL,
  title           TEXT NOT NULL DEFAULT '',
  size            INTEGER NOT NULL DEFAULT 0,
  mtime_ns        INTEGER NOT NULL DEFAULT 0,
  container       TEXT NOT NULL DEFAULT '',
  video_codec     TEXT NOT NULL DEFAULT '',
  audio_codec     TEXT NOT NULL DEFAULT '',
  width           INTEGER NOT NULL DEFAULT 0,
  height          INTEGER NOT NULL DEFAULT 0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  bitrate         INTEGER NOT NULL DEFAULT 0,
  fps             REAL NOT NULL DEFAULT 0,
  status          TEXT NOT NULL DEFAULT 'pending',
  error_class     TEXT NOT NULL DEFAULT '',
  error_message   TEXT NOT NULL DEFAULT '',
  probe_attempts  INTEGER NOT NULL DEFAULT 0,
  missing_since   TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  deleted_at      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_media_library_path ON media(library_id, normalized_path) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_media_library_status ON media(library_id, status);
CREATE INDEX IF NOT EXISTS idx_media_status ON media(status);

CREATE TABLE IF NOT EXISTS scan_tasks (
  id          TEXT PRIMARY KEY,
  library_id  TEXT NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL DEFAULT 'incremental',
  status      TEXT NOT NULL DEFAULT 'pending',
  total       INTEGER NOT NULL DEFAULT 0,
  scanned     INTEGER NOT NULL DEFAULT 0,
  updated     INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  missing     INTEGER NOT NULL DEFAULT 0,
  suspected   INTEGER NOT NULL DEFAULT 0,
  error       TEXT NOT NULL DEFAULT '',
  started_at  TEXT,
  finished_at TEXT,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scan_tasks_library ON scan_tasks(library_id, status);

CREATE TABLE IF NOT EXISTS watch_progress (
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  media_id    TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  position_ms INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0,
  completed   INTEGER NOT NULL DEFAULT 0,
  updated_at  TEXT NOT NULL,
  PRIMARY KEY (user_id, media_id)
);

CREATE TABLE IF NOT EXISTS favorites (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  media_id   TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY (user_id, media_id)
);

CREATE TABLE IF NOT EXISTS reactions (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  media_id   TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL CHECK (kind IN ('like','dislike')),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (user_id, media_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id         TEXT PRIMARY KEY,
  user_id    TEXT NOT NULL DEFAULT '',
  username   TEXT NOT NULL DEFAULT '',
  action     TEXT NOT NULL,
  object     TEXT NOT NULL DEFAULT '',
  ok         INTEGER NOT NULL DEFAULT 1,
  detail     TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at);
