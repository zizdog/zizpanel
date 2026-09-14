package web

import (
	"strings"
	"testing"
)

// TestAppProxyBlockShape 锁死生成出来的 location 形状。
//
// 为什么值得锁：这段文本会直接写进 nginx。少一个 `}` 或写错锚点，
// nginx -t 会拦下来（helper 也会自动回滚），但用户看到的是"生成失败"，
// 而真实原因藏在一大段配置里。这里把关键约束提前钉死。
func TestAppProxyBlockShape(t *testing.T) {
	block := appProxyBlock([]appEntry{{Slug: "iopaint", Name: "IOPaint", Port: 8080}}, "127.0.0.1:8443")
	for _, need := range []string{
		appProxyBegin, appProxyEnd,
		"location = /iopaint {", "return 301 /iopaint/;",
		"location ^~ /iopaint/ {",
		"proxy_pass https://127.0.0.1:8443;",
		// WebSocket 升级必须放行，否则 Kuma / IOPaint 的实时功能坏掉
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $connection_upgrade;",
	} {
		if !strings.Contains(block, need) {
			t.Errorf("生成的块里缺少 %q", need)
		}
	}
	if strings.Count(block, "{") != strings.Count(block, "}") {
		t.Errorf("花括号不配对：\n%s", block)
	}
}

// TestUpsertAppProxyBlockIsIdempotent：重复生成不能累积，且必须落在
// `location / {` 之前 —— 否则 nginx 会先命中 `location /`（前缀匹配下
// 更长的 location 优先，这点是对的，但把块放在 server 块外面会直接语法错）。
func TestUpsertAppProxyBlockIsIdempotent(t *testing.T) {
	orig := `server {
    listen 80;
    location /_panel/ {
        proxy_pass https://127.0.0.1:8443;
    }
    location / {
        try_files $uri $uri/ =404;
    }
}
`
	block := appProxyBlock([]appEntry{{Slug: "iopaint", Name: "IOPaint", Port: 8080}}, "127.0.0.1:8443")
	once, err := upsertAppProxyBlock(orig, block)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := upsertAppProxyBlock(once, block)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Errorf("第二次生成结果不同（不幂等）：\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
	if n := strings.Count(twice, appProxyBegin); n != 1 {
		t.Errorf("标记块应恰好 1 个，实际 %d", n)
	}
	if strings.Index(twice, "location ^~ /iopaint/") > strings.Index(twice, "location / {") {
		t.Error("应用 location 应插在 `location / {` 之前")
	}
	// 原有的面板入口不能被弄丢
	if !strings.Contains(twice, "location /_panel/") {
		t.Error("改写把已有的面板入口弄丢了")
	}
}

// TestUpsertAppProxyBlockRefusesUnpairedMarker：标记不配对时宁可报错，
// 也不要猜着改写（猜错就是把用户的 nginx 配置改坏）。
func TestUpsertAppProxyBlockRefusesUnpairedMarker(t *testing.T) {
	broken := "server {\n" + appProxyBegin + "\n    location / {\n}\n"
	if _, err := upsertAppProxyBlock(broken, appProxyBlock(nil, "127.0.0.1:8443")); err == nil {
		t.Fatal("标记不配对时应报错")
	}
}

// TestUpsertAppProxyBlockRefusesWithoutAnchor：找不到锚点时也要报错，
// 不能把 location 写到 server 块外面。
func TestUpsertAppProxyBlockRefusesWithoutAnchor(t *testing.T) {
	if _, err := upsertAppProxyBlock("listen 80;\n", appProxyBlock(nil, "127.0.0.1:8443")); err == nil {
		t.Fatal("没有插入点时应报错")
	}
}

// TestRefsFromHTML 只挑脚本与样式：图片 404 顶多缺个图标，脚本 404 必然白屏。
func TestRefsFromHTML(t *testing.T) {
	html := `<link rel="icon" href="/favicon.ico">` +
		`<script src="/iopaint/assets/a.js"></script>` +
		`<link href="/iopaint/assets/b.css" rel="stylesheet">` +
		`<img src="/iopaint/assets/c.png">`
	got := refsFromHTML(html)
	if len(got) != 2 {
		t.Fatalf("应挑出 2 个 js/css 引用，实际 %v", got)
	}
	if got[0] != "/iopaint/assets/a.js" || got[1] != "/iopaint/assets/b.css" {
		t.Errorf("挑出来的引用不对：%v", got)
	}
}
