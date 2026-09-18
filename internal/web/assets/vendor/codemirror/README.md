# 内嵌第三方资源

## codemirror/ —— CodeMirror 5.65.16（MIT）

面板的在线编辑器用它（`internal/web/assets/js/files.js`）。**文件未做任何修改**，
只挑了面板真正用到的模式与插件，全部是 cdnjs 上 `codemirror@5.65.16` 的
`*.min.js` / `*.min.css`。

- 来源：https://cdnjs.cloudflare.com/ajax/libs/codemirror/5.65.16/
- 许可证：MIT（见 `LICENSE`，版权归 Marijn Haverbeke 等）
- 体积：约 470KB（39 个文件），随二进制内嵌；**只有打开编辑器时才按需加载**
  （`files.js` 的 `ensureCodeMirror()`），不影响面板首屏。

包含内容：

- 本体：`codemirror.min.js` / `codemirror.min.css`
- 模式（`mode/`）：xml、javascript、css、htmlmixed、php、clike、sql、python、
  go、shell、yaml、markdown、properties（ini/conf）、nginx、dockerfile
- 插件（`addon/`）：search（查找替换）、dialog、matchbrackets、closebrackets、
  continuelist、matchtags、fold*（折叠）、active-line、simplescrollbars、comment
- 主题（`theme/`）：monokai（面板自带的另一种配色；"跟随面板"用的是
  `files.js` 里注入的 `cm-s-zp-panel` 自定义主题）
- `LICENSE`

### 升级步骤

```bash
V=5.65.17   # 新版本号
B=https://cdnjs.cloudflare.com/ajax/libs/codemirror/$V
# 按上面清单把 39 个文件重下到这个目录（路径一一对应），并更新本文件的版本号
```

升级后必须跑一遍 `make ui-audit` 与编辑器手测（见 `ZizPanel-当前状态.md`）：
CodeMirror 的模式依赖关系（php → xml/javascript/css/htmlmixed/clike）写死在
`files.js` 的 `CM_LANGS` 里，大版本升级时要对照它的 `mode/` 目录确认没有新增依赖。
