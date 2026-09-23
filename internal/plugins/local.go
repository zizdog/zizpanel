// 本地插件：`<安装根>/plugins/*.json`（P2）。
//
// 与内建表的区别只有"谁写的"：内建表随面板打包（`builtin/`），本地插件是用户/第三方
// 放在插件目录里的文件。两者走**同一套解析与校验**（Parse → Validate），所以
// "能不能用"的判据完全一致；区别在启用策略：
//
//	· 内建表：随面板发布，默认生效；
//	· 本地插件：**默认关闭**，必须在面板里显式启用（它等于让面板以 root 去装/跑东西）。
//
// 启用状态存在插件目录下的 `enabled.json`（0600）：与插件文件放一起，
// 用户备份/迁移插件目录时状态跟着走；不写进面板配置，免得两份真源。
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalPlugin 是插件目录里的一份声明（含解析失败的那种 —— 失败也要能列出来给用户看）。
type LocalPlugin struct {
	File string `json:"file"`
	Spec *Spec  `json:"spec,omitempty"`
	// Err 非空表示这份文件没通过解析/校验；列表里必须如实展示，不许静默忽略。
	Err string `json:"error,omitempty"`
	// 解析成功时的摘要字段（列表用；解析失败则只有 File 与 Err）
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Icon    string `json:"icon,omitempty"`
	Summary string `json:"summary,omitempty"`
	Source  string `json:"source_kind,omitempty"`
	RunMode string `json:"run_mode,omitempty"`
}

// EnabledFile 是启用状态的落盘文件名。
const EnabledFile = "enabled.json"

// LoadDir 读取插件目录（不存在返回空列表，**不报错**：没装插件是正常状态）。
// 结果按文件名排序，保证界面顺序稳定。
func LoadDir(dir string) []LocalPlugin {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []LocalPlugin
	for _, e := range entries {
		name := e.Name()
		// 点开头的（.remote.json 之类）是面板自己的记录，不是插件声明。
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || name == EnabledFile {
			continue
		}
		full := filepath.Join(dir, name)
		raw, err := os.ReadFile(full)
		if err != nil {
			out = append(out, LocalPlugin{File: name, Err: err.Error()})
			continue
		}
		spec, err := Parse(raw, name)
		if err != nil {
			out = append(out, LocalPlugin{File: name, Err: err.Error()})
			continue
		}
		out = append(out, LocalPlugin{
			File: name, Spec: spec, ID: spec.ID, Name: spec.Name, Icon: spec.Icon,
			Summary: spec.Summary, Source: spec.Source.Kind, RunMode: spec.Run.Mode,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}

// ReadEnabled 读启用状态（缺文件 = 全部默认关闭）。
func ReadEnabled(dir string) map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(dir, EnabledFile))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

// SetEnabled 写入某个插件的启用状态（幂等；目录不存在会创建）。
func SetEnabled(dir, id string, on bool) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return fmt.Errorf("插件目录为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	state := ReadEnabled(dir)
	if on {
		state[id] = true
	} else {
		delete(state, id)
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	// 0600：这只是"哪些插件被启用"，但它能让别人知道本机跑了什么。
	return os.WriteFile(filepath.Join(dir, EnabledFile), append(raw, '\n'), 0o600)
}
