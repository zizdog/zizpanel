-- 0005_seek: 左右键跳转秒数（每用户）。存量 <=0 的值读取时回落默认 10。
ALTER TABLE user_prefs ADD COLUMN seek_seconds INTEGER NOT NULL DEFAULT 10;
