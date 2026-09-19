# 内嵌第三方资源

## codemirror/ —— CodeMirror 5.65.16（MIT）

面板的在线编辑器用它（`internal/web/assets/js/files.js`）。**文件未做任何修改**，
只挑了面板真正用到的模式与插件，全部是 cdnjs 上 `codemirror@5.65.16` 的
`*.min.js` / `*.min.css`。

- 来源：https://cdnjs.cloudflare.com/ajax/libs/codemirror/5.65.16/
- 许可证：MIT（见 `LICENSE`，版权归 Marijn Haverbeke 等；主题 CSS 同属 CodeMirror
  项目，一并按 MIT 授权）
- 体积：约 395KB（41 个资源文件，实测字节数），随二进制内嵌；**只有打开编辑器时才按需加载**
  （`files.js` 的 `ensureCodeMirror()` / `ensureCmTheme()`），不影响面板首屏。

包含内容：

- 本体：`codemirror.min.js` / `codemirror.min.css`
- 模式（`mode/`）：xml、javascript、css、htmlmixed、php、clike、sql、python、
  go、shell、yaml、markdown、properties（ini/conf）、nginx、dockerfile
- 插件（`addon/`）：search（查找替换）、dialog、matchbrackets、closebrackets、
  continuelist、matchtags、fold*（折叠）、active-line、simplescrollbars、comment
- 主题（`theme/`，均为官方文件、未改动，`files.js` 按需加载）：
  - `eclipse.min.css` —— 浅色（面板浅色时"跟随面板"用它）
  - `material-darker.min.css` —— 深色（面板深色时"跟随面板"用它）
  - `monokai.min.css` —— 官方 Monokai（用户在下拉里显式选它时才用）
  - sha256（本次下载核对）：eclipse `e5ea1258e20040cfa83b766eab3fdb8fa8d41de1b3b04ed0c22ec8f321e4fa66`、
    material-darker `36f7867d65852095da9627424ca794ab24b58187ccbdfdf637fda7b57ab417f8`
- `LICENSE`

> **面板不再自研编辑器主题。** 语法着色一律来自上面这些官方主题；
> `files.js` 只注入最小的布局样式（撑满高度、查找框跟随面板）。
> 旧版手写的 `.cm-s-zp-panel` token 颜色与覆盖面板 CSS 变量的 `.zpf-monokai`
> 已删除 —— 后者会把编辑器弹窗里的按钮/输入框一起染色（用户报障）。

### 升级步骤

```bash
V=5.65.17   # 新版本号
B=https://cdnjs.cloudflare.com/ajax/libs/codemirror/$V
# 按上面清单把 41 个文件重下到这个目录（路径一一对应），并更新本文件的版本号
# 主题只需 3 个：theme/{eclipse,material-darker,monokai}.min.css
curl -fsSL -o internal/web/assets/vendor/codemirror/theme/eclipse.min.css $B/theme/eclipse.min.css
curl -fsSL -o internal/web/assets/vendor/codemirror/theme/material-darker.min.css $B/theme/material-darker.min.css
curl -fsSL -o internal/web/assets/vendor/codemirror/theme/monokai.min.css $B/theme/monokai.min.css
```

升级后必须跑一遍 `make ui-audit` 与编辑器手测（见 `ZizPanel-当前状态.md`）：
CodeMirror 的模式依赖关系（php → xml/javascript/css/htmlmixed/clike）写死在
`files.js` 的 `CM_LANGS` 里，大版本升级时要对照它的 `mode/` 目录确认没有新增依赖。
