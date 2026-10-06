# Transmission Next UI（面板给 Transmission 装的中文界面）

- 上游：**Transmission Next UI v0.3.3**，MIT，https://github.com/hisproc/transmission-next-ui
- 上游发布件：`release.zip`（351,269 B）
  （sha256 `7d8fcccf4a73a5616646b945896f6b835e3ed2c7e1ef41530c4ada30286577da`，
  2026-10-06 本机从 GitHub Releases 下载后实算）
- 本目录 = 该 zip 解压后的**原样内容**（`index.html` / `favicon.svg` / `assets/*`），
  只额外放了上游仓库的 `LICENSE`。一个字节都没有改。
- 界面只调 `<origin>/transmission/rpc` 这**一个绝对路径**（bundle 里唯一一处），
  所以它既能被 Transmission 自己从 web 根目录提供服务，也能走面板的 `/transmission/` 代理。
- 面板的用法：安装 Transmission 时把这一份写进 daemon 的 web 根目录
  （`<brew>/share/transmission/public_html`），见 `internal/services/transmission_webui.go`。
- 升级时：换掉本目录里的文件 + 更新 `transmission.go` 的 `transmissionWebUIVersion`
  与本节上面的 sha256，然后在真机上确认 `/transmission/web/` 出来的仍是中文界面。
