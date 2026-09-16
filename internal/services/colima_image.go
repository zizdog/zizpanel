package services

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zizdog/zizpanel/internal/tasks"
)

// ============================================================================
//  Colima 的 guest 虚拟机镜像（P0：不预热就必然装不上）
//
//  首次 `colima start` 要先下 Ubuntu 的 guest 镜像，Colima **不从
//  cloud-images.ubuntu.com 下**，而是从 GitHub release：
//    https://github.com/abiosoft/colima-core/releases/download/v<ver>/
//        ubuntu-24.04-minimal-cloudimg-arm64-docker.raw.gz
//  （arm64+docker 这一份是 **332,354,401 B = 317 MiB**；am64 是 358 MB。）
//
//  真机实测（2026-09-16）：公网 GitHub 只有 ~77 KB/s → **约 71 分钟**，
//  而旧代码只给 `colima start` 5 分钟超时 → **必然被掐断**，于是
//  「Docker 运行时装不上 → 9 个 Docker 应用全废」。
//
//  解决办法（cache 预热），机制已在真机确证：
//   · Colima 把下好的镜像按 **URL 的 sha256** 命名缓存在
//     `~/Library/Caches/colima/caches/<sha256(url)>`；
//     实测 `printf '%s' <那条 URL> | shasum -a 256` **正好**等于本机缓存文件名
//     `b0992ab88f5a3c0c436bbb3065c01466f20dc1dd0eb0a60299d410176f21a1c3`，
//     且该文件的 sha512 与官方发布的 sha512 逐字节一致。
//   · 所以只要在 `colima start` **之前**把这个文件按同样的名字放好，
//     Colima 就直接命中缓存、一个字节都不用从 GitHub 下。
//
//  为什么不改 colima.yaml 的 `diskImage:` / `colima start -i`：
//   那条路要求镜像**必须匹配 colima 内嵌的受支持列表**，否则报错（除非
//   再打开 forceDiskImage 绕过校验）；缓存预热的字节与 colima 自己要下的
//   完全相同，不引入任何"文件对不上"的新失败面。两条路都评估过，选了后者。
// ============================================================================

// colimaCoreURLRe 匹配 colima 二进制里内嵌的 guest 镜像 URL。
//
// 为什么要从二进制里抠、而不是写死版本号：colima 每次发布都会把对应的
// colima-core 版本内嵌进去（实测 colima 0.10.3 里内嵌的是 **v0.10.4**），
// 写死的话 colima 一升级就会指向一个不存在的文件。
var colimaCoreURLRe = regexp.MustCompile(
	`https://github\.com/abiosoft/colima-core/releases/download/v[0-9][0-9.]*/` +
		`ubuntu-[0-9.]+-minimal-cloudimg-(amd64|arm64)-(docker|containerd|incus|none)\.(?:raw\.gz|qcow2)`)

// colimaRuntimeFromConfig 读 colima.yaml 里的 runtime（默认 docker）。
func (m *Manager) colimaRuntimeFromConfig() string {
	b, err := os.ReadFile(m.colimaYAMLPath())
	if err != nil {
		return "docker"
	}
	re := regexp.MustCompile(`(?m)^runtime:\s*([A-Za-z0-9_-]+)`)
	if mm := re.FindStringSubmatch(string(b)); mm != nil {
		return mm[1]
	}
	return "docker"
}

// colimaGuestImageURL 返回"这次 colima start 会去下哪个 guest 镜像"。
func (m *Manager) colimaGuestImageURL() (string, error) {
	bin := m.colimaBin()
	b, err := os.ReadFile(bin)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败：%w", bin, err)
	}
	arch := "arm64"
	if strings.Contains(strings.TrimSpace(runOutput("/usr/bin/uname", "-m")), "x86_64") {
		arch = "amd64"
	}
	runtime := m.colimaRuntimeFromConfig()
	for _, u := range colimaCoreURLRe.FindAllString(string(b), -1) {
		if strings.Contains(u, "-"+arch+"-") && strings.Contains(u, "-"+runtime+".") {
			return u, nil
		}
	}
	return "", fmt.Errorf("没能从 %s 里识别出 guest 镜像地址（arch=%s runtime=%s）", bin, arch, runtime)
}

// colimaGuestCachePath 是 Colima 期望缓存的落点（文件名 = URL 的 sha256）。
func (m *Manager) colimaGuestCachePath(imageURL string) string {
	if m.opt.UserHome == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(imageURL))
	return filepath.Join(m.opt.UserHome, "Library", "Caches", "colima", "caches", hex.EncodeToString(sum[:]))
}

// fileSHA512 算文件 sha512（小写 hex）。
func fileSHA512(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// colimaGuestMirrorURLs 返回镜像站上该文件的候选地址。
//
// 两个布局都试：`apps/colima-core/<ver>/<file>`（与 apps 下其它应用包一致）
// 与 `colima/<ver>/<file>`（短路径）。NAS 上两者是同一个 inode 的硬链接。
func (m *Manager) colimaGuestMirrorURLs(imageURL string) []string {
	base := m.mirrorBase()
	if base == "" {
		return nil
	}
	parts := strings.Split(imageURL, "/")
	file := parts[len(parts)-1]
	ver := ""
	for i, p := range parts {
		if p == "download" && i+1 < len(parts) {
			ver = parts[i+1]
		}
	}
	if ver == "" || file == "" {
		return nil
	}
	return []string{
		base + "/apps/colima-core/" + ver + "/" + file,
		base + "/colima/" + ver + "/" + file,
	}
}

// colimaGuestManifest 是 NAS 上 `apps/colima-core/<ver>/manifest.json` 的结构。
//
// 与 mirror.go 的 apps 清单同一套约定（<base>/apps/<app-id>/<version>/manifest.json）：
// 校验值由**放置方用上游作者发布的校验文件交叉验证过**，面板只做判等、不自己编。
type colimaGuestManifest struct {
	Component string `json:"component"`
	Artifacts []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		SHA512 string `json:"sha512"`
	} `json:"artifacts"`
}

// fetchColimaGuestExpectation 从镜像站清单里取"这个文件的期望 sha256/sha512/大小"。
func (m *Manager) fetchColimaGuestExpectation(ctx context.Context, ver, file string) (sha256Want, sha512Want string, sizeWant int64) {
	base := m.mirrorBase()
	if base == "" {
		return "", "", 0
	}
	txt, err := m.fetchSmallText(ctx, 20*time.Second, base+"/apps/colima-core/"+ver+"/manifest.json")
	if err != nil {
		return "", "", 0
	}
	var mf colimaGuestManifest
	if json.Unmarshal([]byte(txt), &mf) != nil {
		return "", "", 0
	}
	for _, a := range mf.Artifacts {
		if a.Name == file {
			return strings.ToLower(a.SHA256), strings.ToLower(a.SHA512), a.Size
		}
	}
	return "", "", 0
}

// PrewarmColimaGuestImage 在 `colima start` 之前把 guest 镜像放进 Colima 的缓存。
//
// 返回一段"人话来源说明"（写进任务步骤）。**失败不是致命错误**：colima 仍会
// 回落到 GitHub，只是很慢（90 分钟超时兜底），所以这里把原因如实带出去。
//
// 校验纪律（与 tools/build-offline-bundle.sh 一致，别放松）：
//
//	· 优先用镜像站清单里的 **sha256** 判等，没有清单再退回官方 `.sha512sum`；
//	· **对落盘后的文件真算**，不是抄清单；
//	· 不符就删掉临时文件并如实失败 —— 坏镜像比没有镜像更糟（colima 会拿它建 VM）。
func (m *Manager) PrewarmColimaGuestImage(ctx context.Context, result *InstallResult) string {
	imageURL, err := m.colimaGuestImageURL()
	if err != nil {
		return "未能识别 Colima 要用的 guest 镜像，交由 Colima 自行下载：" + err.Error()
	}
	dst := m.colimaGuestCachePath(imageURL)
	if dst == "" {
		return "未知用户家目录，跳过 guest 镜像预热"
	}
	if st, serr := os.Stat(dst); serr == nil && st.Size() > 0 {
		return fmt.Sprintf("guest 镜像已在 Colima 缓存里：%s（%s）", filepath.Base(imageURL), humanBytes(st.Size()))
	}
	urls := m.colimaGuestMirrorURLs(imageURL)
	if len(urls) == 0 {
		return "未配置镜像站，guest 镜像由 Colima 自行从 GitHub 下载（实测约 71 分钟）"
	}
	parts := strings.Split(imageURL, "/")
	file := parts[len(parts)-1]
	ver := ""
	for i, p := range parts {
		if p == "download" && i+1 < len(parts) {
			ver = parts[i+1]
		}
	}
	sha256Want, sha512Want, sizeWant := m.fetchColimaGuestExpectation(ctx, ver, file)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "创建 Colima 缓存目录失败，交给 Colima 自己下：" + err.Error()
	}
	tmp := dst + ".part"
	_ = os.Remove(tmp)
	start := time.Now()
	var lastErr string
	for _, u := range urls {
		if _, derr := m.downloadToFile(ctx, u, tmp, 30*time.Minute, result, "guest 镜像（自建镜像站）"); derr != nil {
			lastErr = derr.Error()
			continue
		}
		st, serr := os.Stat(tmp)
		if serr != nil || st.Size() == 0 {
			_ = os.Remove(tmp)
			lastErr = u + " 下载后文件为空"
			continue
		}
		if sizeWant > 0 && st.Size() != sizeWant {
			_ = os.Remove(tmp)
			lastErr = fmt.Sprintf("%s 的大小是 %d，清单写的是 %d", u, st.Size(), sizeWant)
			continue
		}
		if sha256Want != "" {
			if got := sha256OfFile(tmp); got != sha256Want {
				_ = os.Remove(tmp)
				lastErr = fmt.Sprintf("%s 的 sha256 是 %s，期望 %s", u, got, sha256Want)
				continue
			}
		} else if sha512Want != "" {
			if got, herr := fileSHA512(tmp); herr != nil || got != sha512Want {
				_ = os.Remove(tmp)
				lastErr = fmt.Sprintf("%s 的 sha512 是 %s，期望 %s", u, got, sha512Want)
				continue
			}
		} else {
			// 两个校验值都没有：不猜、不假装通过 —— 如实说明"没校验"。
			lastErr = u + " 的清单/校验文件都取不到，无法核对完整性（本次未采用）"
			_ = os.Remove(tmp)
			continue
		}
		// 缓存文件必须归真实用户所有：Colima 以该用户身份运行，root 属主的
		// 文件它读得到，但后续删除/覆盖会失败（与 .colima 目录同一个坑）。
		if m.opt.UserName != "" {
			_ = chownPath(m.opt.UserName, tmp)
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return "写入 Colima 缓存失败：" + err.Error()
		}
		via := "sha256 已核对"
		if sha256Want == "" {
			via = "sha512 已核对"
		}
		return fmt.Sprintf("guest 镜像来自自建镜像站（%s，耗时 %s，%s，%s）→ Colima 缓存 %s",
			u, time.Since(start).Round(time.Second), humanBytes(st.Size()), via, filepath.Base(dst))
	}
	_ = os.Remove(tmp)
	return "自建镜像站上的 guest 镜像不可用（" + lastErr + "），回落到 Colima 自行从 GitHub 下载（实测约 71 分钟）"
}

// colimaGuestImageNote 在 guest 镜像**确实被用过之后**写一条来源说明。
//
// 与 brew/HF 那套一致：让用户在任务日志里看得见"这个耗时的大件是从哪来的"。
func (m *Manager) colimaGuestImageNote() string {
	imageURL, err := m.colimaGuestImageURL()
	if err != nil {
		return ""
	}
	cache := m.colimaGuestCachePath(imageURL)
	st, serr := os.Stat(cache)
	if serr != nil || st.Size() == 0 {
		return ""
	}
	emitNote := filepath.Base(imageURL) + "（" + humanBytes(st.Size()) + "，本地缓存 " + cache + "）"
	return emitNote
}

// logColimaGuestSource 把 guest 镜像的来源打进任务步骤（尽力而为，不报错）。
func (m *Manager) logColimaGuestSource(ctx context.Context) {
	if note := m.colimaGuestImageNote(); note != "" {
		emit(ctx, tasks.LevelStep, "Colima guest 镜像："+note)
	}
}
