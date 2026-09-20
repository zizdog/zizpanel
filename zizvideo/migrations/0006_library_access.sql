-- 0006_library_access: 指定用户可访问的媒体库。
-- 无行 = 无任何库（fail-closed）；管理员由代码判据直接放行，不写授权行。
-- 不回填存量用户：升级后历史普通用户看不到任何库（有意，见 ITERATION-2 B 节）。
CREATE TABLE IF NOT EXISTS user_libraries (
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  library_id TEXT NOT NULL REFERENCES media_libraries(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  PRIMARY KEY (user_id, library_id)
);
CREATE INDEX IF NOT EXISTS idx_user_libraries_library ON user_libraries(library_id);
