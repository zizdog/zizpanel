package storage

import (
	"path/filepath"
	"testing"

	"github.com/zizdog/zizvideo/internal/domain"
)

func newJob(t *testing.T, db *DB) *domain.JobTask {
	t.Helper()
	j := &domain.JobTask{ID: domain.NewID("job"), Kind: domain.JobKindEpisodeDetect,
		Trigger: domain.JobTriggerManualBatch, Total: 2}
	if err := db.CreateJobTask(j); err != nil {
		t.Fatal(err)
	}
	return j
}

// D.2 后台失败如实：终态写入前校验 —— failed 无 error、degraded 无 reason、
// success 带 error 一律拒绝，且不得把非法状态写进库。
func TestFinishJobTaskHonesty(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cases := []struct {
		name          string
		status        string
		errMsg        string
		degraded      bool
		degradeReason string
		want          error
	}{
		{"failed 无原因", domain.TaskFailed, "", false, "", domain.ErrJobErrorRequired},
		{"failed 只有空白原因", domain.TaskFailed, "   ", false, "", domain.ErrJobErrorRequired},
		{"degraded 无理由", domain.TaskSuccess, "", true, "", domain.ErrJobDegradeReason},
		{"success 带错误", domain.TaskSuccess, "boom", false, "", domain.ErrJobErrorOnSuccess},
	}
	for _, c := range cases {
		j := newJob(t, db)
		if got := db.FinishJobTask(j.ID, c.status, c.errMsg, "{}", 1, c.degraded, c.degradeReason); got != c.want {
			t.Fatalf("%s: err = %v, 期望 %v", c.name, got, c.want)
		}
		got, gerr := db.GetJobTask(j.ID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Status != domain.TaskPending || got.FinishedAt != "" {
			t.Fatalf("%s: 非法终态被写入: %+v", c.name, got)
		}
	}

	// 合法终态照常写入，并保留计数与理由。
	failedJob := newJob(t, db)
	if err := db.FinishJobTask(failedJob.ID, domain.TaskFailed, "1/2 个剧场识别失败：ser_x: 对象不存在",
		`{"failed_total":1}`, 2, true, "2 集未识别"); err != nil {
		t.Fatalf("合法 failed 被拒: %v", err)
	}
	got, err := db.GetJobTask(failedJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TaskFailed || got.Error == "" || !got.Degraded || got.DegradeReason == "" ||
		got.Unidentified != 2 || got.FinishedAt == "" {
		t.Fatalf("failed 终态不完整: %+v", got)
	}
	if err := db.FinishJobTask("job_missing", domain.TaskSuccess, "", "{}", 0, false, ""); err != domain.ErrTaskNotFound {
		t.Fatalf("不存在的 job: err = %v, 期望 ErrTaskNotFound", err)
	}
}

// 重启后遗留的 pending/running 跨库任务必须标记为中断，不许永远"进行中"。
func TestAbandonStaleJobTasks(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "zizvideo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pending := newJob(t, db)
	running := newJob(t, db)
	done := newJob(t, db)
	if err := db.UpdateJobTaskProgress(running.ID, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJobTask(done.ID, domain.TaskSuccess, "", "{}", 0, false, ""); err != nil {
		t.Fatal(err)
	}
	n, err := db.AbandonStaleJobTasks()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("标记数 = %d, 期望 2", n)
	}
	for _, id := range []string{pending.ID, running.ID} {
		got, gerr := db.GetJobTask(id)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if got.Status != domain.TaskInterrupted || got.Error == "" || got.FinishedAt == "" {
			t.Fatalf("任务 %s 未被如实中断: %+v", id, got)
		}
	}
	kept, _ := db.GetJobTask(done.ID)
	if kept.Status != domain.TaskSuccess || kept.Error != "" {
		t.Fatalf("已完成的 job 被改动: %+v", kept)
	}
}
