# AriaNg（面板内置的下载器界面）

- 上游：AriaNg **1.3.14**，MIT，https://github.com/mayswind/AriaNg
- 上游发布件：`AriaNg-1.3.14.zip`
  （sha256 `e00db79b4cabac70f71c2673a6d454c8a92bfa9aa1f37bb00b01b7505f956805`）
- 本目录的来源：npm 包 **`aria-ng-cli@0.1.0`**（MIT）里 `vendor/ariang/`
  那一份，它与它自带的 `ariang.manifest.json` 逐文件 sha256 一致（32/32 核对通过）——
  也就是说内容与上面那个上游 zip 相同。
- 为什么这么绕（不直接下 GitHub）：本仓库的构建机连不上 github.com，
  而 AriaNg 只发 GitHub release zip（npm 上没有官方包）。npm 镜像可达。
- 面板以 `/aria/` 提供这一份界面（见 `internal/web/aria2_web.go`），
  AriaNg 自己的资源引用全是相对路径，所以挂在子路径下不需要改写。
- 升级 AriaNg 时：换掉本目录、核对新版本 manifest、并更新
  `internal/services/aria2.go` 里的 `Aria2UIVersion` 与 `internal/web/assets/ariang/PROVENANCE.md`。
