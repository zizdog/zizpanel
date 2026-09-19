//go:build !darwin

package files

// extraVolumeMounts 是非 darwin 平台的空实现。
//
// 面板只支持 macOS，这个文件的存在只是为了让 `go build` / `go vet` 在别的
// GOOS 下也能编译（例如开发机上跑带 GOOS=linux 的静态检查）。
func extraVolumeMounts() []string { return nil }
