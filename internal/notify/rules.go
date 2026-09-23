package notify

import (
	"fmt"
	"time"
)

// 规则层刻意做成**纯函数**：输入是面板各处取来的"事实快照"，输出是要发的通知。
// 好处：判据可以逐条单测（正反对照），web 层只负责取事实 + 调用 Send。

// ServiceStatus 是"一个服务现在怎么样"的快照。
//
// 为什么带 Status 与 Health 两个维度：只看端口/进程会把"卡死"当正常（aria2 那次），
// 只看健康探针又会把"用户自己停掉的服务"报成异常 —— 两个都要。
type ServiceStatus struct {
	Name     string // 面板记录名（去重键用）
	Display  string // 给人看的名字
	Status   string // running / stopped / error / unknown / unavailable
	HealthOK bool   // 健康探针结论
	Checked  bool   // 探针是否真的跑了
	Detail   string // 出错原因（可为空）
}

// RulesServiceDown 挑出"该跑却没跑好"的服务。
//
// 判定（宁少勿滥，避免把用户故意停掉的服务报成故障）：
//   - Status=error/unavailable ⇒ 报；
//   - Status=running 但健康探针明确不 OK ⇒ 报（这是"卡死"，最该报的一类）；
//   - Status=stopped/unknown ⇒ 不报（用户停的、或还没探测完）。
func RulesServiceDown(items []ServiceStatus) []Event {
	var out []Event
	for _, s := range items {
		switch s.Status {
		case "error", "unavailable":
			out = append(out, Event{
				Key:   "service-down:" + s.Name,
				Title: "服务异常：" + s.Display,
				Body:  firstLine(s.Detail),
				Level: LevelErr,
			})
		case "running":
			if s.Checked && !s.HealthOK {
				out = append(out, Event{
					Key:   "service-unhealthy:" + s.Name,
					Title: "服务在跑但不健康：" + s.Display,
					Body:  firstLine(s.Detail),
					Level: LevelErr,
				})
			}
		}
	}
	return out
}

// CertStatus 是一张证书的快照。
type CertStatus struct {
	Domain   string
	NotAfter time.Time
}

// RulesCertExpiring 挑出"快到期"的证书。days<=0 时用 14 天。
func RulesCertExpiring(items []CertStatus, days int, now time.Time) []Event {
	if days <= 0 {
		days = 14
	}
	var out []Event
	for _, c := range items {
		if c.NotAfter.IsZero() {
			continue
		}
		left := int(c.NotAfter.Sub(now).Hours() / 24)
		if c.NotAfter.After(now) && left <= days {
			out = append(out, Event{
				Key:   "cert-expiring:" + c.Domain,
				Title: fmt.Sprintf("证书 %d 天后到期：%s", left, c.Domain),
				Body:  "自动续期可能失败了（正常应在 30 天前续上）：到「SSL 证书」页点一次续期看真实报错。",
				Level: LevelWarn,
			})
			continue
		}
		if !c.NotAfter.After(now) {
			out = append(out, Event{
				Key:   "cert-expired:" + c.Domain,
				Title: "证书已过期：" + c.Domain,
				Level: LevelErr,
			})
		}
	}
	return out
}

// RulesDiskUsage 磁盘水位。threshold<=0 时用 90（百分比）。
func RulesDiskUsage(mount string, usedPercent, threshold int) []Event {
	if threshold <= 0 {
		threshold = 90
	}
	if usedPercent < threshold {
		return nil
	}
	return []Event{{
		Key:   "disk-high:" + mount,
		Title: fmt.Sprintf("磁盘占用 %d%%：%s", usedPercent, mount),
		Body:  "盘满会让上传/导入大文件直接 500（nginx 缓存请求体需要空间）：先到「磁盘」页清一清。",
		Level: LevelWarn,
	}}
}

// CapEvents 限制单轮通知条数：超出的合并成一条汇总（max<=0 = 不限制）。
//
// 为什么必须限制：没装 Docker 的机器上十来个容器应用会同时报"运行时不可用"，
// 一轮巡检就是十几条通知 —— 用户的第一反应是把通知整个关掉。
func CapEvents(events []Event, max int) []Event {
	if max <= 0 || len(events) <= max {
		return events
	}
	out := make([]Event, 0, max+1)
	out = append(out, events[:max]...)
	out = append(out, Event{
		Key:   "notify-overflow",
		Title: fmt.Sprintf("还有 %d 个问题没有逐条通知", len(events)-max),
		Body:  "去「服务」页看完整列表。",
		Level: LevelWarn,
	})
	return out
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
