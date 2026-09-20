-- 0004_episode: 剧集序列识别（补丁 R1）。season/episode 一起用，NULL = 未识别（绝不猜）。
-- episode_source: filename = 自动识别写入；manual = 手动排序过，自动识别不得覆盖。
ALTER TABLE series_media ADD COLUMN season INTEGER;
ALTER TABLE series_media ADD COLUMN episode INTEGER;
ALTER TABLE series_media ADD COLUMN episode_source TEXT
  CHECK (episode_source IS NULL OR episode_source IN ('filename', 'manual'));
CREATE INDEX IF NOT EXISTS idx_series_media_episode ON series_media(series_id, season, episode);
