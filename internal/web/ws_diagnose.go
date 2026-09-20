package web

import (
	"errors"
	"net/http"
	"strings"
)

// ============================================================================
//  WebSocket 升级失败的可行动结论（坑 219）
//
//  经反代访问面板时，那一层常把逐跳头 Connection/Upgrade 吃掉 ⇒ 握手失败，
//  浏览器只报 1006、拿不到失败握手的响应体。面板必须替用户把失败翻译成
//  "去开 WebSocket 透传"，光记日志等于让用户猜。
// ============================================================================

// wsUpgradeVerdict 是给用户看的结论：Advice 是首行（≤40 字，前端直接显示），
// Detail 收进折叠项。
type wsUpgradeVerdict struct {
	ViaProxy bool   `json:"via_proxy"`
	Advice   string `json:"advice"`
	Detail   string `json:"detail"`
}

const (
	// wsAdviceProxy 逐跳头被反代那一层吃掉了。
	wsAdviceProxy = "检测到反向代理：请开启 WebSocket 透传"
	// wsAdviceDirect 直连面板端口，但浏览器没发起升级。
	wsAdviceDirect = "浏览器未发起 WebSocket 升级，请换浏览器或直连"
	// wsAdviceOther 其余握手失败（版本 / Key / 接管连接）。
	wsAdviceOther = "WebSocket 握手被拒，终端无法建立连接"

	wsDetailProxy = "你正经过反向代理（Nginx / 群晖 / 宝塔 / 路由器 / 隧道）访问面板，" +
		"那一层把 Upgrade / Connection 这两个逐跳头吃掉了。请在那一层开启 WebSocket 透传：" +
		"Nginx 需要 `proxy_set_header Upgrade $http_upgrade;`、" +
		"`proxy_set_header Connection $connection_upgrade;`，" +
		"并在 http 上下文声明 `map $http_upgrade $connection_upgrade { default upgrade; '' close; }`。" +
		"面板自带的规则模板与「修复 Nginx 环境」已包含这些，请核对上游那一层。"
	wsDetailDirect = "请求没经过反向代理，也没有带 WebSocket 升级头。" +
		"请在浏览器里换用最新版 Chrome / Safari / Edge，或用面板直连地址（https://<主机>:8443/<后缀>）访问；" +
		"也确认没有企业代理、杀毒软件在改写请求头。"
)

// hasProxyTrail 判断请求是否带着反代痕迹。
// 只看"是不是经反代来的"，不猜是哪一层。
func hasProxyTrail(r *http.Request) bool {
	for _, k := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "X-Real-IP"} {
		if strings.TrimSpace(r.Header.Get(k)) != "" {
			return true
		}
	}
	return false
}

// wsHeaderEaten 判断错误是否属于"逐跳头缺失 / 被改写"这一类。
func wsHeaderEaten(err error) bool {
	return errors.Is(err, errWSNoConnectionUpgrade) || errors.Is(err, errWSUpgradeNotWebsocket)
}

// wsDiagnoseRequest 在"升级失败已经发生"的前提下，只用请求头判断该怪谁。
// 普通 HTTP 请求（诊断接口）永远不带 Connection: Upgrade，所以只看反代痕迹。
func wsDiagnoseRequest(r *http.Request) wsUpgradeVerdict {
	if hasProxyTrail(r) {
		return wsUpgradeVerdict{ViaProxy: true, Advice: wsAdviceProxy, Detail: wsDetailProxy}
	}
	return wsUpgradeVerdict{Advice: wsAdviceDirect, Detail: wsDetailDirect}
}

// wsUpgradeFailure 把一次失败的握手翻译成结论。
func wsUpgradeFailure(r *http.Request, err error) wsUpgradeVerdict {
	if wsHeaderEaten(err) {
		return wsDiagnoseRequest(r)
	}
	return wsUpgradeVerdict{
		ViaProxy: hasProxyTrail(r),
		Advice:   wsAdviceOther,
		Detail:   "握手被拒原因：" + err.Error(),
	}
}

// handleTerminalWSDiagnose 在终端 WS 连不上时给前端一条可行动结论。
//
// 浏览器 onclose 只有 1006、读不到失败握手的响应体，所以前端连不上后
// 用普通 HTTP 请求这里把结论取回去显示。
func (s *Server) handleTerminalWSDiagnose(w http.ResponseWriter, r *http.Request) {
	v := wsDiagnoseRequest(r)
	ok(w, map[string]any{"via_proxy": v.ViaProxy, "advice": v.Advice, "detail": v.Detail})
}
