# zizvideo 运行接口契约（两个接口都必须在，改动=破坏兼容）

zizvideo 以两种方式运行，**共用**同一份二进制、config.json、数据目录与端口 7766；
两种方式**互斥**（同一时刻只能有一个在跑）。无论怎么重构，下面这些都不许丢。

## ① 面板托管入口（走面板的 TCC 授权）
- launchd 启动的是**面板二进制**：`<面板二进制> zizvideo-supervise --user <u> --zizvideo <bin> --config <cfg> --home <h> --listen 127.0.0.1:7766 --log-dir <d>`
- supervisor 以 root fork 后 `SysProcAttr.Credential` setuid 到真实用户（**不经 sudo**）。
- plist 由面板写（`/Library/LaunchDaemons/cn.zizpanel.zizvideo.plist`，**不写 UserName**）。
- zizvideo 必须能作为该 supervisor 的子进程正常启动（参数与上面逐字兼容）。

## ② 独立入口（走 zizvideo 自己的 TCC 授权）
- launchd 启动的是 **zizvideo 自己的二进制**：`zizvideo --supervise --user <u> --config <cfg> --listen 127.0.0.1:7766`
- 自写系统 plist（`com.zizvideo.server`，`UserName=<u>`、`RunAtLoad`+`KeepAlive`、`HOME`），开机即起、不需登录。
- 用自己的固定证书签名（identifier `com.zizvideo.server`）并由安装器导入信任 ⇒ 授权授一次、升级不失效。

## 两种模式共用的对外契约
- `--version` 输出必须含版本号（面板/安装器按它复核）；`GET /healthz` 返回 ok；只绑回环。
- config.json 字段（`media_allow_roots` 等）与数据目录 `~/Library/Application Support/zizvideo/`。
- 安装位 `/opt/zizvideo/bin/zizvideo`；端口 **7766**。
- 版本真源在镜像：`apps/zizvideo/manifest.json`（`app/latest/assets[{name,version,arch,sha256,size}]`）
  + 产物 `zizvideo_<版本>_darwin_arm64`（裸二进制，只发 arm64）。
