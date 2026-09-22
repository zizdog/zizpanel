-- 0010_series_dir: 剧场自己的目录（上传落点）。空 = 尚未确定，按标题+库根推导。
ALTER TABLE series ADD COLUMN dir_path TEXT NOT NULL DEFAULT '';
