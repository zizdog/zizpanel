-- 0008_job_tasks: 跨库后台任务（一键识别 / 扫描后自动识别）。
-- 不复用 scan_tasks：它 library_id NOT NULL + 外键指向单库，装不下跨库任务（ITERATION-2 0 节）。
-- 如实语义（A.5）：status='failed' ⇒ error 非空；degraded=1 ⇒ degrade_reason 非空（写库前校验）。
CREATE TABLE IF NOT EXISTS job_tasks (
  id             TEXT PRIMARY KEY,
  kind           TEXT NOT NULL,
  trigger        TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','running','success','failed','interrupted')),
  total          INTEGER NOT NULL DEFAULT 0,
  processed      INTEGER NOT NULL DEFAULT 0,
  updated        INTEGER NOT NULL DEFAULT 0,
  failed         INTEGER NOT NULL DEFAULT 0,
  manual_skipped INTEGER NOT NULL DEFAULT 0,
  unidentified   INTEGER NOT NULL DEFAULT 0,
  degraded       INTEGER NOT NULL DEFAULT 0 CHECK (degraded IN (0,1)),
  degrade_reason TEXT NOT NULL DEFAULT '',
  error          TEXT NOT NULL DEFAULT '',
  summary        TEXT NOT NULL DEFAULT '',
  started_at     TEXT,
  finished_at    TEXT,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_tasks_created ON job_tasks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_job_tasks_status ON job_tasks(status);
