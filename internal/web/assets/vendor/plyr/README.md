# 内嵌第三方资源

## plyr/ —— Plyr 3.8.4（MIT）

面板文件管理用它播放视频/音频（`internal/web/assets/js/files.js` 的 `ensurePlyr()`）。
选它的理由：MIT、体积小（约 150KB）、视频与音频同一套 UI、零运行时依赖、纯原生解码
（不做转码），与"无构建步骤 + 不联网"的前端取向一致。

- 来源：https://registry.npmjs.org/plyr/-/plyr-3.8.4.tgz （`npm pack plyr@3.8.4`）
- 许可证：MIT（见 `LICENSE.md`，版权归 Sam Potts）
- 文件（sha256 为入库时实测）：
  - `plyr.min.js` 112620B `1a7b9bfe7537c447b392068ec2e8629ce5ec35d54c7d436a1a1b307c87eaecc6`
  - `plyr.css` 32455B `c96a7fff7cac0a7f65ffcd9a7c668130a30a904bc3c7f4eabfe76e71c2c6a772`
  - `plyr.svg` 5669B `de12fcd36f9e90e5a1b7cf027a3dbfc7d90ea2902737a8c5acf1ea5c40d6769f`（控件图标 sprite）
- 唯一改动：删掉 `plyr.min.js` 末尾的 `//# sourceMappingURL=plyr.min.js.map`
  （不 vendor `.map`，留着会让 devtools 404）；其余文件一字未改。
- **运行时绝不联网**：Plyr 默认 `iconUrl` 指向 `https://cdn.plyr.io/...`，`files.js` 里已覆盖为
  本地 `plyr.svg`（有门禁扫 `cdn.plyr.io`）。
- 按需加载：只有真的点开音视频才注入 css/js，不影响面板首屏（同 CodeMirror 的做法）。

### 升级步骤

```bash
cd /tmp && rm -rf plyrpack && mkdir plyrpack && cd plyrpack
npm pack plyr@3.9.0   # 新版本号
tar xzf plyr-3.9.0.tgz
# 覆盖 dist/{plyr.min.js,plyr.css,plyr.svg,LICENSE.md}，
# 重新删掉 plyr.min.js 末尾的 sourceMappingURL 一行，并更新本文件的版本号与 sha256。
```

升级后确认 `plyr.svg` 里的图标 id 与 `files.js` 的控件清单对得上（默认 `iconPrefix=plyr`），
并跑一遍 `go test ./internal/web/ -run 'Media|Plyr' -count=1`。
