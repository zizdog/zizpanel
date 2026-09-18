package priv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
//  Nginx 性能参数的改写（宝塔式表单的后端）
//
//  这里锁四件事：
//    ① 幂等：同一个值写两遍，文件一字不变（用户点两次保存不该动 nginx）；
//    ② 就地改：已有指令替换那一行，不新增重复指令
//       （实测：同一上下文里 gzip 出现两次 → nginx 报 `"gzip" directive is duplicate`
//        直接拒绝启动，所以这条不是洁癖）；
//    ③ 缺则插：nginx.conf 里没有的指令插到**正确的上下文**（main / events / http）；
//    ④ 真实 nginx 接受：改完的配置能通过 `nginx -t`，且 `nginx -T` 里读得到新值。
// ============================================================================

const sampleConf = `# nginx.conf（复刻 Homebrew 的形态）
worker_processes auto;
error_log /opt/homebrew/var/log/nginx/error.log warn;
pid /opt/homebrew/var/run/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include       mime.types;
    default_type  application/octet-stream;
    keepalive_timeout 65;
    gzip on;
    server_names_hash_bucket_size 64;
    include /opt/homebrew/etc/nginx/vhosts/*.conf;
}
`

func TestApplyNginxTuningReplacesInPlace(t *testing.T) {
	v := DefaultTuning()
	v.ClientMaxBodySizeMB = 512
	v.Gzip = false
	v.GzipCompLevel = 5
	v.WorkerConnections = 2048
	v.KeepaliveTimeout = 60

	out, err := ApplyNginxTuning(sampleConf, v)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	for _, want := range []string{
		"client_max_body_size 512m;",
		"gzip off;",
		"gzip_comp_level 5;",
		"worker_connections 2048;",
		"keepalive_timeout 60;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("改写结果里缺少 %q：\n%s", want, out)
		}
	}
	// 旧值必须消失（否则就是"写了两份"）
	if strings.Contains(out, "gzip on;") {
		t.Errorf("旧的 `gzip on;` 没被替换掉：\n%s", out)
	}
	if strings.Count(out, "worker_connections") != 1 {
		t.Errorf("worker_connections 应当只出现一次：\n%s", out)
	}
	if !strings.Contains(out, "include /opt/homebrew/etc/nginx/vhosts/*.conf;") {
		t.Errorf("其它内容不许被动：\n%s", out)
	}
}

func TestApplyNginxTuningIsIdempotent(t *testing.T) {
	v := DefaultTuning()
	v.ClientMaxBodySizeMB = 256
	v.GzipMinLengthKB = 16
	once, err := ApplyNginxTuning(sampleConf, v)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := ApplyNginxTuning(once, v)
	if err != nil {
		t.Fatal(err)
	}
	if once != twice {
		t.Errorf("同一个值写两遍结果必须一致（否则用户每点一次保存都会改文件）：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", once, twice)
	}
}

func TestApplyNginxTuningInsertsIntoRightContext(t *testing.T) {
	out, err := ApplyNginxTuning(sampleConf, DefaultTuning())
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	// gzip_min_length / client_* 原本没有：必须插进 http 块（不是 main、不是 events）
	httpStart := strings.Index(out, "http {")
	eventsIdx := strings.Index(out, "events {")
	if eventsIdx < 0 || httpStart < 0 {
		t.Fatalf("样例配置结构异常:\n%s", out)
	}
	for _, d := range []string{"gzip_min_length", "client_header_buffer_size", "client_body_buffer_size"} {
		idx := strings.Index(out, d)
		if idx < 0 {
			t.Fatalf("%s 没有被插入：\n%s", d, out)
		}
		if idx < httpStart {
			t.Errorf("%s 应插在 http 块里（实际插在它之前）：\n%s", d, out)
		}
	}
	// worker_processes 在 main 上下文，不能被搬到 http 里
	wp := strings.Index(out, "worker_processes")
	if wp > eventsIdx {
		t.Errorf("worker_processes 必须在 events 之前（main 上下文）：\n%s", out)
	}
}

// TestApplyNginxTuningCommentsDuplicate：同一上下文里重复的指令必须被处理掉。
//
// 用户自己（或旧版本面板）可能已经写了两份 —— 直接替换第一处会让第二处继续
// 触发 `directive is duplicate`，nginx 起不来。
func TestApplyNginxTuningCommentsDuplicate(t *testing.T) {
	dup := `worker_processes auto;
events {
    worker_connections 512;
}
http {
    client_max_body_size 1m;
    client_max_body_size 2m;
}
`
	v := DefaultTuning()
	v.ClientMaxBodySizeMB = 128
	out, err := ApplyNginxTuning(dup, v)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "client_max_body_size 128m;"); n != 1 {
		t.Errorf("新值应只出现一次，实际 %d 次：\n%s", n, out)
	}
	// 第二处必须被整行注释掉（带说明、且保留原内容便于回查），
	// 不能留着让 nginx 拒绝启动。
	if !strings.Contains(out, "# ZizPanel：这一条是重复指令，已注释") {
		t.Errorf("重复指令应被注释并说明原因（否则 nginx 报 duplicate 拒绝启动）：\n%s", out)
	}
	if !strings.Contains(out, "原内容：client_max_body_size 2m;") {
		t.Errorf("注释里要保留原内容（用户回查时要看得见自己写过什么）：\n%s", out)
	}
	if strings.Contains(out, "\n    client_max_body_size 2m;") {
		t.Errorf("重复的旧指令不能仍然生效：\n%s", out)
	}
}

func TestNginxTuningValuesReadsCurrent(t *testing.T) {
	vals, found := NginxTuningValues(sampleConf)
	if vals.WorkerProcesses != "auto" || !found["worker_processes"] {
		t.Errorf("worker_processes 应读到 auto：%+v found=%v", vals, found)
	}
	if vals.WorkerConnections != 1024 || vals.KeepaliveTimeout != 65 {
		t.Errorf("events/http 里的数值没读对：%+v", vals)
	}
	if !vals.Gzip {
		t.Errorf("gzip on 应读成 true：%+v", vals)
	}
	if found["client_max_body_size_mb"] {
		t.Errorf("配置里没有 client_max_body_size，found 不该为 true（界面据此标注「用的是出厂默认」）：%v", found)
	}
	if vals.ClientMaxBodySizeMB != 1 {
		t.Errorf("没有该指令时用 nginx 出厂默认 1m：%+v", vals)
	}
}

func TestValidateTuning(t *testing.T) {
	if err := ValidateTuning(DefaultTuning()); err != nil {
		t.Errorf("出厂默认值必须合法，实际 %v", err)
	}
	bad := DefaultTuning()
	bad.ClientMaxBodySizeMB = 0
	if err := ValidateTuning(bad); err == nil {
		t.Error("0 MB 应当被拒绝")
	}
	bad = DefaultTuning()
	bad.GzipCompLevel = 12
	if err := ValidateTuning(bad); err == nil {
		t.Error("gzip_comp_level=12 应当被拒绝")
	}
	bad = DefaultTuning()
	bad.WorkerProcesses = "many"
	if err := ValidateTuning(bad); err == nil {
		t.Error("worker_processes=many 应当被拒绝")
	}
}

// TestApplyNginxTuningAcceptedByRealNginx：真实 nginx 必须接受改写后的配置。
//
// 只断言"文本里有那行字"是不够的 —— 指令写错上下文，nginx 会直接拒绝加载。
func TestApplyNginxTuningAcceptedByRealNginx(t *testing.T) {
	nginxBin := "/opt/homebrew/bin/nginx"
	if _, err := os.Stat(nginxBin); err != nil {
		t.Skip("未安装 nginx，跳过真实配置校验")
	}
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	runDir := filepath.Join(dir, "run")
	for _, d := range []string{logDir, runDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf := `worker_processes auto;
error_log ` + logDir + `/error.log warn;
pid ` + runDir + `/nginx.pid;
events {
    worker_connections 1024;
}
http {
    include /opt/homebrew/etc/nginx/mime.types;
    access_log ` + logDir + `/access.log;
    client_body_temp_path ` + runDir + `/body;
    proxy_temp_path ` + runDir + `/proxy;
    fastcgi_temp_path ` + runDir + `/fastcgi;
    uwsgi_temp_path ` + runDir + `/uwsgi;
    scgi_temp_path ` + runDir + `/scgi;
    keepalive_timeout 65;
    gzip on;
}
`
	v := DefaultTuning()
	v.ClientMaxBodySizeMB = 300
	v.Gzip = false
	v.GzipCompLevel = 4
	v.GzipMinLengthKB = 2
	v.ServerNamesHashBucketSize = 256
	v.ClientHeaderBufferSizeKB = 16
	v.ClientBodyBufferSizeKB = 256
	v.WorkerConnections = 4096
	out, err := ApplyNginxTuning(conf, v)
	if err != nil {
		t.Fatalf("改写失败: %v", err)
	}
	confPath := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(confPath, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if o, err := exec.Command(nginxBin, "-c", confPath, "-t").CombinedOutput(); err != nil {
		t.Fatalf("改写后的配置没通过真实 nginx -t：\n%s\n--- 配置 ---\n%s", o, out)
	}
	// `nginx -T` 打印真实加载的配置：里面必须能看到新值（这才叫"生效值"）。
	dump, err := exec.Command(nginxBin, "-c", confPath, "-T").CombinedOutput()
	if err != nil {
		t.Fatalf("nginx -T 失败：%v\n%s", err, dump)
	}
	eff, _ := NginxTuningValues(string(dump))
	if eff.ClientMaxBodySizeMB != 300 {
		t.Errorf("nginx -T 里读到的 client_max_body_size 不是 300m：%+v", eff)
	}
	if eff.Gzip {
		t.Errorf("nginx -T 里 gzip 应当已关：%+v", eff)
	}
	if eff.WorkerConnections != 4096 {
		t.Errorf("nginx -T 里 worker_connections 不是 4096：%+v", eff)
	}
	t.Logf("真实 nginx 接受改写：client_max_body_size=%dm gzip=%v comp=%d conns=%d",
		eff.ClientMaxBodySizeMB, eff.Gzip, eff.GzipCompLevel, eff.WorkerConnections)
}

// TestApplyNginxTuningRefusesSingleLineBlock：把块写在一行时**如实拒绝**，绝不猜。
//
// 写进错误的上下文会让 nginx 直接拒绝启动（例如把 worker_connections 放到 http 里），
// 所以这种情况下宁可报错让用户手工处理。
func TestApplyNginxTuningRefusesSingleLineBlock(t *testing.T) {
	oneLine := "worker_processes auto;\nevents { worker_connections 512; }\nhttp {}\n"
	v := DefaultTuning()
	v.ClientMaxBodySizeMB = 64
	_, err := ApplyNginxTuning(oneLine, v)
	if err == nil {
		t.Fatal("单行块结构下应当拒绝改写（写错上下文会让 nginx 起不来）")
	}
	if !strings.Contains(err.Error(), "不认") {
		t.Errorf("错误信息要说明「面板不认这种结构」并给出出路，实际 %v", err)
	}
}
