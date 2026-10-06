package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// flakySource 模拟 soft 挂载的网络盘：读到某个偏移就返回 ETIMEDOUT，重新打开后能接着读。
// （真机形态：NFS `soft,timeo=10,retrans=2`，1MB 大块读偶尔超时一次。）
type flakySource struct {
	data        []byte
	off         int64
	failAt      int64 // 读到这个偏移就报超时（<=0 表示不再失败）
	failures    int   // 已经失败过几次（每次重开算一轮）
	maxFailures int
}

func (f *flakySource) Read(p []byte) (int, error) {
	if f.failAt > 0 && f.off >= f.failAt && f.failures < f.maxFailures {
		f.failures++
		f.failAt = 0 // 只在这一轮的这个地方失败一次
		return 0, syscall.ETIMEDOUT
	}
	if f.off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *flakySource) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	case io.SeekEnd:
		f.off = int64(len(f.data)) + offset
	}
	return f.off, nil
}

func (f *flakySource) Close() error { return nil }

// TestCopyResumesAfterNetworkReadTimeout 是 2026-10-06 用户报障（网络盘拷大文件
// `read …: operation timed out`）的门禁：
//   - 读超时 ⇒ **从已写偏移续传**，最终内容逐字节正确；
//   - 只在可恢复的网络类错误上续传，其它错误立刻如实失败；
//   - 重试次数超上限要如实写进错误里（并说明已传多少）。
//
// 负向对照：把 copyFileProgress 退回"一次读失败就返回"，第一条断言立刻红。
func TestCopyResumesAfterNetworkReadTimeout(t *testing.T) {
	payload := bytes.Repeat([]byte("ABCDEFGH"), 400*1024) // ~3.2MB，跨多个 1MB 块
	prev := copyOpenSrcFn
	t.Cleanup(func() { copyOpenSrcFn = prev })

	t.Run("读超时后续传成功且内容一致", func(t *testing.T) {
		calls := 0
		copyOpenSrcFn = func(string) (io.ReadSeekCloser, error) {
			calls++
			// 第一轮：读到 1.5MB 处超时；之后正常。
			src := &flakySource{data: payload, maxFailures: 1}
			if calls == 1 {
				src.failAt = 1500 * 1024
			}
			return src, nil
		}
		dst := filepath.Join(t.TempDir(), "out.bin")
		b := &batchProgress{}
		if err := copyFileProgress(context.Background(), "/net/src", dst, 0o600, b); err != nil {
			t.Fatalf("读超时后应自动续传成功，实际失败：%v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("续传后内容不对：期望 %d 字节，实际 %d 字节", len(payload), len(got))
		}
		if calls < 2 {
			t.Fatalf("应重新打开过源（续传），实际只开了 %d 次", calls)
		}
		if b.retries != 1 || !strings.Contains(b.note, "续传") {
			t.Errorf("续传必须如实上报（retries=%d note=%q）", b.retries, b.note)
		}
		if b.doneBytes != int64(len(payload)) {
			t.Errorf("进度字节数应与文件大小一致（不能把重传的部分算两遍）：%d vs %d",
				b.doneBytes, len(payload))
		}
	})

	t.Run("非网络类错误不重试、直接失败", func(t *testing.T) {
		calls := 0
		copyOpenSrcFn = func(string) (io.ReadSeekCloser, error) {
			calls++
			return &flakySource{data: payload, failAt: 1, maxFailures: 1}, nil
		}
		dst := filepath.Join(t.TempDir(), "out.bin")
		// EIO 属于可恢复类；这里换成"权限"这类不该重试的错误。
		copyOpenSrcFn = func(string) (io.ReadSeekCloser, error) {
			calls++
			return errReader{err: os.ErrPermission}, nil
		}
		start := time.Now()
		err := copyFileProgress(context.Background(), "/net/src", dst, 0o600, &batchProgress{})
		if err == nil {
			t.Fatal("权限错误必须失败")
		}
		if calls != 1 {
			t.Errorf("不可恢复的错误不该重试（开了 %d 次）", calls)
		}
		if time.Since(start) > 3*time.Second {
			t.Error("不可恢复的错误不该走退避等待")
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
func (e errReader) Seek(int64, int) (int64, error) {
	return 0, nil
}
func (e errReader) Close() error { return nil }
