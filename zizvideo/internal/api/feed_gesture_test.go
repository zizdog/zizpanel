package api_test

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 门禁：一次手势只前进一条。
//
// 直接跑真实 feed.js 导出的 createGestureGate，喂进"一次手势里触发了多次切换"
// 的事件序列（旧 bug 的现场）：只要有一次手势推进两条，断言就失败。
func TestGestureGateAdvancesOneItemPerGesture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("本机没有 node，跳过前端手势门禁")
	}
	feedPath, err := filepath.Abs(filepath.Join("..", "web", "assets", "js", "feed.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(feedPath); err != nil {
		t.Fatalf("找不到 feed.js: %v", err)
	}
	moduleURL := (&url.URL{Scheme: "file", Path: feedPath}).String()

	harness := fmt.Sprintf(`
import assert from "node:assert/strict";
import { createGestureGate } from %q;

// 1) 一次触摸手势内多次触发（touchend + 其它回调叠加）也只推进一条
{
  const gate = createGestureGate({ quietMs: 400 });
  gate.begin(1000);
  const steps = [gate.take(1000, 1), gate.take(1001, 1), gate.take(1200, 1)];
  assert.deepEqual(steps, [1, 0, 0], "一次触摸手势只能前进一条");
  gate.settle();
}

// 2) 滚轮连发 + 惯性（16ms 一发）只算一次手势
{
  const gate = createGestureGate({ quietMs: 400 });
  let steps = 0;
  for (let i = 0; i < 12; i += 1) {
    const now = 2000 + i * 16;
    gate.input(now);
    if (gate.take(now, 1)) steps += 1;
  }
  assert.equal(steps, 1, "一次滚轮手势只能前进一条");
  gate.settle();
}

// 3) 位移动画未结束时，新手势也不能叠加
{
  const gate = createGestureGate({ quietMs: 0 });
  gate.begin(3000);
  assert.equal(gate.take(3000, 1), 1);
  gate.begin(3500);
  assert.equal(gate.take(3500, 1), 0, "动画未结束不得再切一条");
  gate.settle();
  gate.begin(4000);
  assert.equal(gate.take(4000, 1), 1, "动画结束后新手势可以继续");
}

// 4) 手势之间静默足够久 → 允许下一条
{
  const gate = createGestureGate({ quietMs: 400 });
  gate.begin(5000);
  assert.equal(gate.take(5000, 1), 1);
  gate.settle();
  gate.input(5500);
  assert.equal(gate.take(5500, 1), 1);
}

// 5) 反向上滑同样服从同一把锁
{
  const gate = createGestureGate({ quietMs: 400 });
  gate.begin(6000);
  assert.equal(gate.take(6000, -1), -1);
  assert.equal(gate.take(6001, -1), 0, "反向也不能一次跳两条");
}

console.log("gesture gate OK: 一次手势只前进一条");
`, moduleURL)

	dir := t.TempDir()
	script := filepath.Join(dir, "gesture_gate_test.mjs")
	if err := os.WriteFile(script, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("手势门禁失败: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
