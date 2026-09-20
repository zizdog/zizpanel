-- 0007_default_visible: 默认可见库（自助注册继承）+ 授权来源标记。
-- 默认可见库集合 = default_for_new_users=1 且未软删；空集合 = 未设置（fail-closed）。
ALTER TABLE media_libraries ADD COLUMN default_for_new_users INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_libraries_default_new ON media_libraries(default_for_new_users)
  WHERE deleted_at IS NULL;

-- source 只做显示/审计，绝不参与判据（resolveScope 只认 user_libraries 的行数）。
ALTER TABLE user_libraries ADD COLUMN source TEXT NOT NULL DEFAULT 'admin'
  CHECK (source IN ('admin', 'default'));
