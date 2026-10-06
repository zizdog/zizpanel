# noVNC（面板内嵌的网页远程桌面客户端）

- 上游：**noVNC v1.5.0**，MPL-2.0（见同目录 `LICENSE.txt`），https://github.com/novnc/noVNC
- 本目录 = 上游发布包 `v1.5.0.tar.gz`（sha256 见下）里的 `core/` 与 `vendor/pako/`，
  **一个字节都没有改**；`LICENSE.txt` 是上游仓库的同名文件。
  发布包 sha256：`6a73e41f98388a5348b7902f54b02d177cb73b7e5eb0a7a0dcf688cc2c79b42a`
  （2026-10-06 本机 `curl -L https://github.com/novnc/noVNC/archive/refs/tags/v1.5.0.tar.gz` 后
  `shasum -a 256` 实算）
- 面板用它做"网页远程桌面"：浏览器里 `import RFB from '<资产根>/novnc/core/rfb.js'`，
  经面板的 WebSocket 桥（`internal/web/api_remote_desktop.go`）连本机 `127.0.0.1:5900`
  （macOS 的屏幕共享服务）。
- 为什么选它：它**原生支持 Apple 的 ARD 认证（安全类型 30）与 `RFB 003.889` 版本协商**
  （见 `core/rfb.js` 的 `securityTypeARD` 与 `case "003.889"`），所以不需要在 macOS 上
  打开"VNC 旧版密码"（那条会把机器认证降级成一个可爆破的 8 位密码）—— 直接用 Mac 账号 + 密码。
- 升级时：换掉 `core/` 与 `vendor/`、更新上面的版本与 sha256，然后按
  `internal/web/api_remote_desktop.go` 的自检（`rfb-check`）在真机上确认安全类型仍然可用。
