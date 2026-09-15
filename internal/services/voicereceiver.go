package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
//  TtsVoice 音色样本接收端（receiver.py）
//
//  契约来源：网站侧项目 zizdog.cn 的
//  usr/plugins/TtsVoice/HANDOFF-TO-MINI.md —— 那份文档是自包含的，
//  receiver.py 的完整代码就在里面。这里**逐字节照搬**，不做任何"优化"：
//
//    · 文档第 6 节明确写了"不要修改 receiver.py 的接口路径 / 参数名 /
//      响应字段"，网站插件按那份契约调用，改了会直接失联。
//    · 保持字节一致还有一个好处：网站侧可以用哈希核对这台机器上跑的是
//      不是同一份代码（版本漂移是这种跨项目对接最容易出的问题）。
//
//  它同时干两件事：
//    1. 接收网站上传的音色样本（落到本机，因为上游只认本地文件路径）
//    2. 作为**带鉴权的反向代理**对外提供 /v1/*，把请求转发给
//       127.0.0.1:8880 上那个没有鉴权的 mlx-audio
//
//  网络拓扑（这是这次改动最核心的一点）：
//    8880 只监听 127.0.0.1  ← 上游没鉴权，绝不能对外
//    8899 监听 0.0.0.0      ← 唯一对外入口，共享密钥鉴权
//
//  与文档的一处刻意不同：文档用 ~/Library/LaunchAgents（用户级 agent），
//  这里注册为**系统级 LaunchDaemon + UserName**。理由与 Qwen 服务相同：
//  用户级服务要有人登录才跑，而这是一台不接显示器、无人登录的服务器。
// ============================================================================

//go:embed voice-receiver.py
var voiceReceiverPy []byte

// 内置默认音色（v1.7.1）。
//
// 为什么要有它：以前"没上传过音色就用不了"—— 每个新站点、每台新装的机器都得先
// 自己录一段，否则合成直接被拒。现在装完接收端就自带一份可用的默认音色，
// 没指定音色的调用方（插件、curl、别的程序）直接就能用。
//
// 合规：这份样本由用户提供（"龙安灵心"，7.68 秒，24kHz/单声道/16bit PCM），
// 已确认可用于本项目；ref.txt 是它实际念的那句话，克隆时给对参考文字更稳。
//
//go:embed assets/default-voice.wav
var defaultVoiceWav []byte

// defaultVoiceRefText 是内置音色的参考文字（音频里念的那句话，逐字）。
const defaultVoiceRefText = "今天过得怎么样，不管发生了什么，开心的还是难受的，都跟我说说吧，我一直都在这里陪着你呢呢。"

// defaultVoiceSource 是内置音色所在的来源标识：调用方不指定音色时就走它。
const defaultVoiceSource = "default"

// builtinVoiceMarker 记录"这个 default 样本是我们自己放的"，用于升级时更新它
// 而**不覆盖用户自己上传的 default 样本**。
const builtinVoiceMarker = "builtin-voice.json"

// 与 receiver.py 的文件名约定逐字对齐（两边不一致就会出现"面板写了、接收端没读"）
const (
	REF_FILENAME      = "ref.wav"
	REF_TEXT_FILENAME = "ref.txt"
)

const (
	receiverLabel = "com.zizdog.voicereceiver"
	receiverPort  = 8899
	// receiverTokenPrefix 是自动生成密钥的前缀（与交接文档 5.2 步的格式一致）
	receiverTokenPrefix = "ttsv-"
	// 与 Qwen 一致：代理对外的端口，上游在 127.0.0.1:8880
	qwenUpstream = "http://127.0.0.1:8880"
)

// receiverPaths 是接收端的目录约定（与文档一致）。
type receiverPaths struct {
	Dir     string // ~/tts/voice-receiver
	Script  string // ~/tts/voice-receiver/receiver.py
	Samples string // ~/tts/voice-samples
	Jobs    string // ~/tts/jobs（v1.3.0 的作业队列落盘目录）
	Keys    string // ~/tts/voice-receiver/keys.json（v1.6.0 多密钥 + 额度）
	Usage   string // ~/tts/voice-receiver/usage.json（v1.6.0 用量统计，接收端写）
	OutLog  string
	ErrLog  string
	Plist   string // /Library/LaunchDaemons/com.zizdog.voicereceiver.plist
}

func (m *Manager) receiverPaths() receiverPaths {
	home := m.opt.UserHome
	if home == "" {
		home = "/Users/" + m.opt.UserName
	}
	dir := filepath.Join(home, "tts", "voice-receiver")
	return receiverPaths{
		Dir:     dir,
		Script:  filepath.Join(dir, "receiver.py"),
		Samples: filepath.Join(home, "tts", "voice-samples"),
		Jobs:    filepath.Join(home, "tts", "jobs"),
		Keys:    filepath.Join(dir, "keys.json"),
		Usage:   filepath.Join(dir, "usage.json"),
		OutLog:  filepath.Join(dir, "launchd.out.log"),
		ErrLog:  filepath.Join(dir, "launchd.err.log"),
		Plist:   "/Library/LaunchDaemons/" + receiverLabel + ".plist",
	}
}

// ============================================================================
//  多密钥 + 每密钥额度（v1.6.0）
//
//  为什么不再用 plist 里的单个 --token：多密钥、每密钥额度、用量统计都要求
//  "改一次就生效"。plist 是 root 拥有的，改它要重写 XML 并重启守护进程 ——
//  每加一个网站的密钥就重启一次正在跑合成的服务，不可接受。
//
//  所以：密钥与额度放 keys.json（面板写、接收端按 mtime 热加载），
//  用量由接收端写 usage.json、面板通过 GET /usage 读。
//  接收端仍接受 plist 里的 --token 作为兼容回退；重新部署时会被迁移成
//  keys.json 里的一条 default 记录，plist 不再写 --token。
// ============================================================================

// VoiceKey 是 keys.json 里的一条密钥。
type VoiceKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Key        string `json:"key"`
	QuotaChars int64  `json:"quota_chars"` // 0 = 不限
	Enabled    bool   `json:"enabled"`
	Created    int64  `json:"created"`
}

// VoiceKeyView 是"密钥 + 实时用量"的合并视图（面板列表用）。
type VoiceKeyView struct {
	VoiceKey
	UsedChars int64 `json:"used_chars"`
	// ReservedChars 是"在跑的作业占用的额度"（v1.7.0）；剩余额度已经把它扣掉了。
	ReservedChars  int64            `json:"reserved_chars"`
	RemainingChars int64            `json:"remaining_chars"`
	Unlimited      bool             `json:"unlimited"`
	TodayChars     int64            `json:"today_chars"`
	WeekChars      int64            `json:"week_chars"`
	Requests       int64            `json:"requests"`
	Jobs           int64            `json:"jobs"`
	AudioBytes     int64            `json:"audio_bytes"`
	FirstUsed      int64            `json:"first_used"`
	LastUsed       int64            `json:"last_used"`
	Days           map[string]int64 `json:"days,omitempty"`
	// Deleted 表示这条密钥已被删掉、只剩下历史用量（面板显示成"已删除"）。
	Deleted bool `json:"deleted,omitempty"`
	// Legacy 表示这条来自 plist 的兼容 --token（还没迁移，面板里只读）。
	Legacy bool `json:"legacy,omitempty"`
}

// voiceKeysDoc 是 keys.json 的顶层结构。
type voiceKeysDoc struct {
	Version int        `json:"version"`
	Updated int64      `json:"updated"`
	Keys    []VoiceKey `json:"keys"`
}

// VoiceKeyID 校验并返回一个安全的密钥 id（与接收端的 valid_key_id 同规则）。
func VoiceKeyID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("密钥 id 不能为空")
	}
	if len(id) > 64 {
		return "", fmt.Errorf("密钥 id 过长（最多 64 字符）")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", fmt.Errorf("密钥 id 只能包含字母、数字、- 和 _")
		}
	}
	return id, nil
}

// LoadVoiceKeys 读 keys.json；文件不存在返回空表（不是错误）。
func (m *Manager) LoadVoiceKeys() ([]VoiceKey, error) {
	b, err := os.ReadFile(m.receiverPaths().Keys)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取密钥表失败: %w", err)
	}
	var doc voiceKeysDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("密钥表格式不对（%s）: %w", m.receiverPaths().Keys, err)
	}
	return doc.Keys, nil
}

// SaveVoiceKeys 原子写 keys.json，并把它交给运行接收端的那个用户。
//
// 权限 0600 且归属运行用户：接收端要以该用户身份读取，而里面有明文密钥。
func (m *Manager) SaveVoiceKeys(keys []VoiceKey) error {
	p := m.receiverPaths()
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	doc := voiceKeysDoc{Version: 1, Updated: time.Now().Unix(), Keys: keys}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.Keys + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入密钥表失败: %w", err)
	}
	if err := os.Rename(tmp, p.Keys); err != nil {
		return fmt.Errorf("安装密钥表失败: %w", err)
	}
	// 归属改为运行用户（面板以 root 运行，接收端以该用户运行）
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, p.Keys); err != nil {
			return fmt.Errorf("设置密钥表归属失败: %w", err)
		}
	}
	return nil
}

// NewVoiceKey 生成一条新密钥（id 与密钥值都在面板侧生成）。
//
// 密钥值留空时生成 `ttsv-<32 hex>`（与交接文档 5.2 步的格式一致）。
func (m *Manager) NewVoiceKey(name, value string, quotaChars int64, enabled bool) (VoiceKey, error) {
	if quotaChars < 0 {
		return VoiceKey{}, fmt.Errorf("额度不能是负数")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		h, err := randomHex(16)
		if err != nil {
			return VoiceKey{}, err
		}
		value = receiverTokenPrefix + h
	}
	if err := ValidateReceiverToken(value); err != nil {
		return VoiceKey{}, err
	}
	id, err := randomHex(4)
	if err != nil {
		return VoiceKey{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名密钥"
	}
	return VoiceKey{
		ID:         "k-" + id,
		Name:       name,
		Key:        value,
		QuotaChars: quotaChars,
		Enabled:    enabled,
		Created:    time.Now().Unix(),
	}, nil
}

// voiceAuthKey 取一把可用于调用接收端自身的密钥（面板读来源/用量时用）。
//
// 优先用 keys.json 里第一条启用的；还没迁移的机器回退到 plist 的 --token。
func (m *Manager) voiceAuthKey() (string, error) {
	if keys, err := m.LoadVoiceKeys(); err == nil {
		for _, k := range keys {
			if k.Enabled && k.Key != "" {
				return k.Key, nil
			}
		}
	}
	p := m.receiverPaths()
	if _, err := os.Stat(p.Plist); err != nil {
		return "", fmt.Errorf("接收端还没部署过（找不到 %s）", p.Plist)
	}
	if t := m.existingReceiverToken(p); t != "" {
		return t, nil
	}
	return "", nil // 未启用鉴权：空密钥照常可调
}

// ensureVoiceKeys 保证密钥表存在：不存在就从现有 plist 的 --token（或新生成的
// 值）建一条 default 记录。返回是否新建、以及当前第一条可用密钥。
//
// 迁移的关键一步：老机器上网站用的是 plist 里那把密钥，必须原样搬进
// keys.json，否则"升级面板"就等于"网站全部 401"。
func (m *Manager) ensureVoiceKeys(p receiverPaths, preferred string) (bool, string, error) {
	keys, err := m.LoadVoiceKeys()
	if err != nil {
		return false, "", err
	}
	if len(keys) > 0 {
		for _, k := range keys {
			if k.Enabled && k.Key != "" {
				return false, k.Key, nil
			}
		}
		return false, "", nil
	}

	value := strings.TrimSpace(preferred)
	if value == "" {
		value = m.existingReceiverToken(p)
	}
	key, err := m.NewVoiceKey("默认密钥", value, 0, true)
	if err != nil {
		return false, "", err
	}
	// id 固定为 default：接收端的兼容回退也把 --token 记在 default 名下，
	// 这样迁移前后历史用量落在同一个桶里，统计不会断成两截。
	key.ID = "default"
	if err := m.SaveVoiceKeys([]VoiceKey{key}); err != nil {
		return false, "", err
	}
	return true, key.Key, nil
}

// AddVoiceKey 添加一条密钥。值不能与已有密钥重复（重复等于悄悄共用额度）。
func (m *Manager) AddVoiceKey(name, value string, quotaChars int64, enabled bool) (VoiceKey, error) {
	key, err := m.NewVoiceKey(name, value, quotaChars, enabled)
	if err != nil {
		return VoiceKey{}, err
	}
	keys, err := m.LoadVoiceKeys()
	if err != nil {
		return VoiceKey{}, err
	}
	for _, k := range keys {
		if k.Key == key.Key {
			return VoiceKey{}, fmt.Errorf("这个密钥值已经存在（%s），换一个或复制已有的", k.Name)
		}
	}
	keys = append(keys, key)
	if err := m.SaveVoiceKeys(keys); err != nil {
		return VoiceKey{}, err
	}
	return key, nil
}

// UpdateVoiceKey 改一条密钥的名称/额度/启用状态/密钥值。
//
// 不允许把**最后一条启用的密钥**停用——那等于一键把所有人关在门外，
// 而且从界面上看不出为什么。
func (m *Manager) UpdateVoiceKey(id string, name *string, quotaChars *int64,
	enabled *bool, value *string) (VoiceKey, error) {
	keys, err := m.LoadVoiceKeys()
	if err != nil {
		return VoiceKey{}, err
	}
	idx := -1
	for i, k := range keys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return VoiceKey{}, fmt.Errorf("找不到密钥 %s（可能已被删除，刷新一下）", id)
	}

	if name != nil {
		if n := strings.TrimSpace(*name); n != "" {
			keys[idx].Name = n
		}
	}
	if quotaChars != nil {
		if *quotaChars < 0 {
			return VoiceKey{}, fmt.Errorf("额度不能是负数")
		}
		keys[idx].QuotaChars = *quotaChars
	}
	if value != nil {
		v := strings.TrimSpace(*value)
		if v == "" {
			h, err := randomHex(16)
			if err != nil {
				return VoiceKey{}, err
			}
			v = receiverTokenPrefix + h
		}
		if err := ValidateReceiverToken(v); err != nil {
			return VoiceKey{}, err
		}
		for _, k := range keys {
			if k.Key == v && k.ID != id {
				return VoiceKey{}, fmt.Errorf("这个密钥值已经存在（%s）", k.Name)
			}
		}
		keys[idx].Key = v
	}
	if enabled != nil {
		if !*enabled {
			remaining := 0
			for _, k := range keys {
				if k.ID != id && k.Enabled {
					remaining++
				}
			}
			if remaining == 0 {
				return VoiceKey{}, fmt.Errorf("至少要保留一条启用的密钥，否则所有网站都会立刻 401")
			}
		}
		keys[idx].Enabled = *enabled
	}

	if err := m.SaveVoiceKeys(keys); err != nil {
		return VoiceKey{}, err
	}
	return keys[idx], nil
}

// DeleteVoiceKey 删除一条密钥（历史用量仍留在 usage.json 里）。
func (m *Manager) DeleteVoiceKey(id string) error {
	keys, err := m.LoadVoiceKeys()
	if err != nil {
		return err
	}
	out := make([]VoiceKey, 0, len(keys))
	found := false
	enabledLeft := 0
	for _, k := range keys {
		if k.ID == id {
			found = true
			if k.Enabled {
				for _, o := range keys {
					if o.ID != id && o.Enabled {
						enabledLeft++
					}
				}
				if enabledLeft == 0 {
					return fmt.Errorf("这是最后一条启用的密钥，删掉会让所有网站立刻 401；" +
						"请先添加并启用一条新的")
				}
			}
			continue
		}
		out = append(out, k)
	}
	if !found {
		return fmt.Errorf("找不到密钥 %s（可能已被删除，刷新一下）", id)
	}
	return m.SaveVoiceKeys(out)
}

// InstallVoiceReceiver 部署接收端，并返回共享密钥与对外地址。
//
// 密钥的处理：已存在就复用（换掉会让网站那边立刻失联），
// 不存在才生成。生成后既要写进 plist，也要回显给调用方 ——
// 网站插件要把同一个值填到 openaiKey 与 refUploadToken 两处。
// ReceiverOptions 是部署接收端时用户可做的选择。
type ReceiverOptions struct {
	// Token 是指定密钥；为空且 NoAuth=false 时自动生成
	Token string
	// NoAuth 为 true 时使用空密钥部署（= 不鉴权）。
	// Host 是监听地址。空 = 0.0.0.0（网站可能在别的机器上，这是历史默认值）。
	// 本机自己用（网站与接收端同一台）时应传 127.0.0.1 —— 交接文档明确这么要求：
	// "本机只给网站用，127.0.0.1 就够"，没有必要把接收端暴露到局域网。
	Host string

	// receiver.py 的语义：--token 为空时 hmac 比较的是空串，
	// 不带鉴权头的请求直接通过，且 /voice/health 会报 auth:false。
	// 这意味着**任何人都能往这台机器投放文件**，所以只能在内网自用。
	NoAuth bool
}

func (m *Manager) InstallVoiceReceiver(ctx context.Context, result *InstallResult, opt ReceiverOptions) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("部署接收端需要以 root 运行")
	}
	if m.opt.UserName == "" {
		return fmt.Errorf("无法确定运行该服务的真实用户")
	}

	// ---- 0. 命令行开发者工具 ----
	// 接收端脚本用 /usr/bin/python3 跑；全新 macOS 上那只是个占位程序
	// （跑它会弹"安装开发者工具"）。不先装 CLT，plist 注册出来的服务起不来，
	// 而报错只会在 launchd 日志里，联想不到是"缺开发者工具"。
	if err := m.EnsureCLT(ctx, result); err != nil {
		return err
	}

	p := m.receiverPaths()

	// ---- 1. 目录与脚本 ----
	for _, d := range []string{p.Dir, p.Samples, p.Jobs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建 %s 失败: %w", d, err)
		}
	}
	if err := chownTree(m.opt.UserName, filepath.Join(p.Dir, "..")); err != nil {
		return fmt.Errorf("设置目录归属失败: %w", err)
	}

	// 脚本内容始终以面板内置的这份为准：它逐字节来自交接文档，
	// 若机器上是别的内容（手工改过 / 旧版本），这里覆盖并明确告知。
	if old, err := os.ReadFile(p.Script); err != nil || string(old) != string(voiceReceiverPy) {
		if err != nil {
			result.step(ctx, "已写入 receiver.py（来自交接文档）")
		} else {
			result.Steps = append(result.Steps,
				"检测到 receiver.py 与交接文档的版本不一致，已替换为文档版本")
		}
		if werr := os.WriteFile(p.Script, voiceReceiverPy, 0o755); werr != nil {
			return fmt.Errorf("写入 receiver.py 失败: %w", werr)
		}
	} else {
		result.step(ctx, "receiver.py 已是最新（与交接文档一致）")
	}
	_ = chownTree(m.opt.UserName, p.Dir)

	// ---- 1b. 内置默认音色 ----
	// 装完就有可用音色：全新机器不必先录一段（没指定音色的调用直接用 default）。
	if err := m.provisionDefaultVoice(ctx, result, p); err != nil {
		return err
	}
	_ = chownTree(m.opt.UserName, p.Samples)

	// ---- 2. 密钥表 ----
	// v1.6.0：密钥与额度由 keys.json 管理（接收端热加载）。这里做两件事：
	//   a) 表还不存在 → 用现有 plist 的 --token 建一条 default（迁移，网站不中断）
	//   b) 用户显式指定了密钥 → 只有表里一条 key 都没有时才采用
	token := strings.TrimSpace(opt.Token)
	if opt.NoAuth {
		token = ""
		result.Steps = append(result.Steps,
			"按选择**不启用鉴权**（密钥表为空）——同内网任何人都能上传文件与调用合成")
	}
	fresh, primaryKey, err := m.ensureVoiceKeys(p, token)
	if err != nil {
		return err
	}
	switch {
	case fresh && token != "":
		result.step(ctx, "已按你指定的密钥建立密钥表")
	case fresh:
		result.step(ctx, "已建立密钥表（迁移了原有的共享密钥，网站不用改）")
	case len(token) > 0:
		result.step(ctx, "密钥表已存在，保持原有密钥不变（新增/改额度请用「调用密钥」）")
	default:
		result.step(ctx, "密钥表已存在，保持原有密钥不变")
	}
	keys, _ := m.LoadVoiceKeys()
	enabled := 0
	for _, k := range keys {
		if k.Enabled {
			enabled++
		}
	}
	result.step(ctx, fmt.Sprintf("密钥表 %s：%d 条（启用 %d 条）", p.Keys, len(keys), enabled))

	// ---- 3. 系统级服务 ----
	// 监听地址：只接受 127.0.0.1 / 0.0.0.0 两个值（其他值交给 Python 报错没有意义，
	// 而且这里要写进 root 拥有的 plist，宁可先校验）。
	//
	// 没显式指定时**沿用已有 plist 里的地址**：重新部署是升级 receiver.py 的正常
	// 操作（面板的"部署"按钮默认不传 host），如果这时回落 0.0.0.0，就会把一台
	// 本来只监听 127.0.0.1 的机器悄悄暴露到局域网 —— 用户点的是"升级"，
	// 得到的却是"扩大了暴露范围"。
	bindHost := strings.TrimSpace(opt.Host)
	if bindHost == "" {
		if existing := m.existingReceiverHost(p); existing != "" {
			bindHost = existing
			result.step(ctx, "沿用原有监听地址 "+bindHost+"（重新部署不改变暴露范围）")
		} else {
			bindHost = "0.0.0.0"
		}
	}
	if bindHost != "0.0.0.0" && bindHost != "127.0.0.1" {
		return fmt.Errorf("监听地址只支持 0.0.0.0（对局域网开放）或 127.0.0.1（仅本机）")
	}
	if err := m.applyReceiverPlist(ctx, result, p, bindHost); err != nil {
		return err
	}

	// ---- 4. 验证：只认真的返回了 auth:true，并且**真拿密钥调一次** ----
	// 只看 auth:true 不够：健康检查是"配置层面"的回答。这里再用密钥打一次
	// /jobs（200）+ 用错误密钥打一次（403），才算鉴权真的生效。
	wantAuth := !opt.NoAuth && (enabled > 0)
	if !waitJSONBoolValue(ctx, fmt.Sprintf("http://127.0.0.1:%d/voice/health", receiverPort),
		"auth", wantAuth, 20*time.Second) {
		result.Warning = fmt.Sprintf("接收端已注册，但 /voice/health 未返回 auth:true。请看日志：%s", p.ErrLog)
		result.step(ctx, "警告："+result.Warning)
		return nil
	}
	result.step(ctx, "接收端已就绪并启用鉴权")

	if primaryKey != "" {
		ok, detail := m.verifyReceiverKey(ctx, primaryKey)
		if !ok {
			result.Warning = "接收端起来了，但用密钥访问 /jobs 没通过：" + detail +
				"。请看日志 " + p.ErrLog
			result.step(ctx, "警告："+result.Warning)
			return nil
		}
		result.step(ctx, "已用密钥实测 /jobs（200），并确认错误密钥被拒（403）")
	}

	if err := m.RegisterInstalledService(ctx, receiverLabel, "TtsVoice 音色接收端", "🔐", "ai", receiverPort); err != nil {
		result.step(ctx, "（自动登记到服务管理失败："+err.Error()+"）")
	}

	// ---- 5. 把网站那边要的两样东西直接列出来 ----
	//
	// 插件从 2026-09-13 起会**自动推导**接收端地址与密钥：
	//   refUploadUrl   = openaiBaseUrl 去掉结尾的 /v1
	//   refUploadToken = openaiKey
	// 所以这里只给这两项，不再让用户填四个字段（多填一处就多一个填错的机会）。
	host := m.primaryIP()
	key := primaryKey
	if key == "" {
		key = "（留空 —— 未启用鉴权）"
	}
	result.Steps = append(result.Steps,
		"",
		"┌─────────────────────────────────────────────┐",
		"│  请记录下面两项，填进网站 TtsVoice 插件     │",
		"└─────────────────────────────────────────────┘",
		"  本机地址 = "+host,
		"  共享密钥 = "+(map[bool]string{true: "（见下方可复制区块）", false: "（未启用鉴权）"})[key != ""],
		"",
		"插件里这样填：",
		fmt.Sprintf("  openaiBaseUrl = http://%s:%d/v1", host, receiverPort),
		"  openaiKey     = 见下方可复制区块",
		"",
		"  接收端地址与密钥会自动从上面两项推导，不用另外填。",
		"  多个网站请用「服务管理 → 音色接收端 → 详情 → 🔑 调用密钥」各发一把，",
		"  并给每把设置额度（单位：字）—— 一把密钥泄露不影响其它站点。",
		"  （密钥只放在结果里、不写进步骤文本：步骤会进操作审计，密钥不该留在那里。）",
	)
	result.Token = primaryKey
	result.Address = host
	return nil
}

// verifyReceiverKey 用指定密钥真打一次 /jobs，并确认错误密钥被拒。
//
// 为什么要真打：历史上 launchctl 的退出码与 /voice/health 的 auth:true
// 都谎报过成功（服务起来了但用的是旧配置）。鉴权这种"配错了就全站失联"
// 的东西，必须用一次真实调用证明。
func (m *Manager) verifyReceiverKey(ctx context.Context, key string) (bool, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d/jobs?status=queued", receiverPort)

	code, err := httpGetStatus(ctx, url, key)
	if err != nil {
		return false, err.Error()
	}
	if code != 200 {
		return false, fmt.Sprintf("带正确密钥返回 %d", code)
	}

	badCode, err := httpGetStatus(ctx, url, "ttsv-definitely-not-a-key")
	if err != nil {
		return false, "错误密钥测试失败：" + err.Error()
	}
	if badCode == 200 {
		return false, "错误密钥也能访问，鉴权没有真正生效"
	}
	return true, ""
}

// ============================================================================
//  内置默认音色（v1.7.1）
//
//  目标：全新安装面板 → 在面板里装接收端 → **不配置任何音色**也能直接合成。
//  做法：安装/重新部署接收端时，把内置样本写到 <样本目录>/default/ref.wav，
//  并把它的参考文字写成 ref.txt（接收端在调用方没给 ref_text 时读它）。
//
//  不覆盖用户的东西：只有当 default 样本不存在、或者存在且**是我们自己放的**
//  （有 builtin-voice.json 标记且 sha 对得上）时才写。用户自己上传过 default
//  样本的机器，升级时不会被悄悄换掉。
// ============================================================================

// builtinVoiceInfo 是标记文件的内容。
type builtinVoiceInfo struct {
	Source  string `json:"source"`
	SHA256  string `json:"sha256"`
	Size    int    `json:"size"`
	RefText string `json:"ref_text"`
	Updated int64  `json:"updated"`
}

// builtinVoiceSHA 返回内置样本的 sha256（十六进制）。
func builtinVoiceSHA() string {
	sum := sha256.Sum256(defaultVoiceWav)
	return hex.EncodeToString(sum[:])
}

// provisionDefaultVoice 保证 <样本目录>/default/ref.wav 可用。
//
// 返回一段给用户看的说明（会进任务日志）。
func (m *Manager) provisionDefaultVoice(ctx context.Context, result *InstallResult, p receiverPaths) error {
	dir := filepath.Join(p.Samples, defaultVoiceSource)
	target := filepath.Join(dir, REF_FILENAME)
	refTextPath := filepath.Join(dir, REF_TEXT_FILENAME)
	markerPath := filepath.Join(dir, builtinVoiceMarker)

	wantSHA := builtinVoiceSHA()

	// 已有样本：是我们放的才更新，用户自己传的一律不碰
	if _, err := os.Stat(target); err == nil {
		var info builtinVoiceInfo
		marked := false
		if b, rerr := os.ReadFile(markerPath); rerr == nil {
			if json.Unmarshal(b, &info) == nil && info.Source == defaultVoiceSource {
				marked = true
			}
		}
		if !marked {
			result.step(ctx, "保留了你自己上传的 default 音色样本（内置默认音色不会覆盖它）")
			return nil
		}
		if info.SHA256 == wantSHA {
			result.step(ctx, "内置默认音色已是最新（default/ref.wav）")
			return nil
		}
		result.step(ctx, "内置默认音色已更新（替换掉旧的内置样本）")
	} else {
		result.step(ctx, "写入内置默认音色（default/ref.wav），没指定音色的调用可以直接用")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建默认音色目录失败: %w", err)
	}
	// 原子写：先 .part 再 rename，避免被读到半个文件
	tmp := target + ".part"
	if err := os.WriteFile(tmp, defaultVoiceWav, 0o644); err != nil {
		return fmt.Errorf("写入内置音色失败: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("安装内置音色失败: %w", err)
	}
	// 参考文字：接收端在调用方没给 ref_text 时读它
	if err := os.WriteFile(refTextPath, []byte(defaultVoiceRefText+"\n"), 0o644); err != nil {
		return fmt.Errorf("写入内置音色参考文字失败: %w", err)
	}
	info := builtinVoiceInfo{
		Source:  defaultVoiceSource,
		SHA256:  wantSHA,
		Size:    len(defaultVoiceWav),
		RefText: defaultVoiceRefText,
		Updated: time.Now().Unix(),
	}
	body, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(markerPath, body, 0o644); err != nil {
		return fmt.Errorf("写入内置音色标记失败: %w", err)
	}
	if m.opt.UserName != "" {
		if err := chownTree(m.opt.UserName, filepath.Join(p.Samples, defaultVoiceSource)); err != nil {
			return fmt.Errorf("设置默认音色归属失败: %w", err)
		}
	}
	// 验证真的落盘且可读（不看退出码看文件）
	got, err := os.ReadFile(target)
	if err != nil || len(got) != len(defaultVoiceWav) {
		return fmt.Errorf("内置音色写入后校验失败（读到 %d 字节，期望 %d）", len(got), len(defaultVoiceWav))
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != wantSHA {
		return fmt.Errorf("内置音色写入后 sha256 不一致")
	}
	return nil
}

// applyReceiverPlist 写 plist 并（重新）装载守护进程。
//
// v1.6.0 起 plist 里不再写 --token：密钥与额度在 keys.json（热加载），
// plist 只负责告诉接收端那个文件在哪。这样"加密钥/改额度"不需要重启服务。
func (m *Manager) applyReceiverPlist(ctx context.Context, result *InstallResult,
	p receiverPaths, host string) error {
	// 管理密钥：已有就原样保留，没有才生成（换掉不会影响网站，但会让面板
	// 暂时管不了"清零用量"）。与调用密钥分开是故意的 —— 调用密钥在网站手里。
	adminToken := m.existingReceiverAdminToken(p)
	if adminToken == "" {
		h, err := randomHex(16)
		if err != nil {
			return err
		}
		adminToken = "zpa-" + h
		result.step(ctx, "已生成管理密钥（面板用它清零用量；网站插件拿不到）")
	} else {
		result.step(ctx, "沿用原有管理密钥")
	}
	plist := receiverPlist(p, m.opt.UserName, host, adminToken)
	if err := os.WriteFile(p.Plist+".tmp", []byte(plist), 0o644); err != nil {
		return fmt.Errorf("写入 plist 失败: %w", err)
	}
	if err := os.Rename(p.Plist+".tmp", p.Plist); err != nil {
		return fmt.Errorf("安装 plist 失败: %w", err)
	}
	if err := m.bootstrapService(ctx, receiverLabel, p.Plist); err != nil {
		return err
	}
	result.step(ctx, "已注册为系统级后台服务（开机自启、不依赖用户登录）")
	return nil
}

// existingReceiverHost 从已有 plist 里读回监听地址；读不到返回 ""。
//
// 重新部署时**必须**保留原监听地址：本机是 127.0.0.1（只给同机的网站用），
// mini 是 0.0.0.0（网站在别的机器上）。顺手改成默认值等于悄悄把它暴露到局域网。
func (m *Manager) existingReceiverHost(p receiverPaths) string {
	b, err := os.ReadFile(p.Plist)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		// 精确匹配 <string>--host</string>，不要用 Contains：
		// plist 里允许 XML 注释，注释里提到 --host 是正常的，
		// 用 Contains 会把注释当成参数行、然后从注释后面取到一个错的值。
		if strings.TrimSpace(ln) == "<string>--host</string>" && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			v = strings.TrimPrefix(v, "<string>")
			v = strings.TrimSuffix(v, "</string>")
			if v == "127.0.0.1" || v == "0.0.0.0" {
				return v
			}
		}
	}
	return ""
}

// existingReceiverAdminToken 从已有 plist 里读回管理密钥；读不到返回 ""。
//
// 与调用密钥不同：管理密钥只在面板与接收端之间使用，网站插件拿不到，
// 所以**重新部署时必须原样保留**（换掉不影响网站，但会让面板管不了用量重置）。
func (m *Manager) existingReceiverAdminToken(p receiverPaths) string {
	b, err := os.ReadFile(p.Plist)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "<string>--admin-token</string>" && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			v = strings.TrimPrefix(v, "<string>")
			v = strings.TrimSuffix(v, "</string>")
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// ValidateReceiverToken 校验用户自定义的共享密钥。
//
// 为什么必须校验：这个值会被写进 **root 拥有的 plist XML**，
// 里面出现 & < > " 会让 plist 语法坏掉（launchd 直接拒绝装载，接收端起不来）。
// 所以只允许 URL/密钥里常见的安全字符，并且限制长度。
func ValidateReceiverToken(token string) error {
	if token == "" {
		return fmt.Errorf("密钥不能为空")
	}
	if len(token) < 8 || len(token) > 128 {
		return fmt.Errorf("密钥长度应在 8–128 个字符之间")
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("密钥只能包含字母、数字、连字符、下划线和点（不能有空格或 & < > 等字符）")
		}
	}
	return nil
}

// httpGetStatus 用指定密钥 GET 一个地址，只看状态码。
func httpGetStatus(ctx context.Context, url, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		req.Header.Set("X-TtsVoice-Token", token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// existingReceiverToken 从已有 plist 里读回密钥，读不到返回空串。
func (m *Manager) existingReceiverToken(p receiverPaths) string {
	b, err := os.ReadFile(p.Plist)
	if err != nil {
		return ""
	}
	// 取 --token 后面那个字符串
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		// 同上：必须精确匹配参数行（模板注释里会出现 --token 这个词）
		if strings.TrimSpace(ln) == "<string>--token</string>" && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			v = strings.TrimPrefix(v, "<string>")
			v = strings.TrimSuffix(v, "</string>")
			// 这里曾经要求 v 以 "ttsv-" 开头 —— 那是面板自动生成密钥的格式，
			// 但用户完全可以自定义密钥（ValidateReceiverToken 允许）。
			// 自定义密钥读回来就成了空串：换密钥会"又生成一个"、来源管理也
			// 无法鉴权。密钥就是 --token 的那个值，不该按前缀猜。
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// ============================================================================
//  音色来源（receiver v1.5.0）
//
//  背景：v1.4.0 的接收端把所有站点的样本都写成同一个 <dir>/ref.wav，
//  后传的覆盖先传的 —— 多站点共用一份样本时，用户听到的是"别的站的音色"，
//  而且没有任何提示。v1.5.0 改成 <dir>/<source>/ref.wav。
//
//  面板这边**不自己解析/转码音频**，只做两件事：
//    · 列来源：问接收端 GET /voice/sources（单一事实来源，口径不会漂）
//    · 换/删来源：把文件转给接收端 POST /voice（校验与归一化只有一处实现）
//  密钥从 plist 里读（与"更改共享密钥"同一处），用户不需要再填一次。
// ============================================================================

// VoiceSource 是一条来源记录（对应接收端 /voice/sources 的一项）。
type VoiceSource struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	// HasRefText/RefText：该来源目录里有没有 ref.txt（内置默认音色带一份）
	HasRefText bool   `json:"has_ref_text"`
	RefText    string `json:"ref_text"`
	// Builtin：这是面板内置的默认音色（安装接收端时写进去的）
	Builtin  bool    `json:"builtin"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
	SHA256   string  `json:"sha256"`
	Modified int64   `json:"modified"`
	LastUsed int64   `json:"last_used"`
	Legacy   bool    `json:"legacy"`
}

// SanitizeVoiceSource 与 receiver.py 的 sanitize_source() 保持同一套规则。
//
// 两处必须一致：面板用它拼删除请求的路径，接收端用它拼目录名。
// 不一致会出现"面板删的是 A、接收端理解成 B"。
func SanitizeVoiceSource(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func (m *Manager) receiverBaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", receiverPort)
}

// receiverToken 取一把可用于调用接收端自身的密钥；未部署时返回错误
// （而不是空串 —— 空串在接收端语义里是"不鉴权"，会把"没部署"伪装成"权限正常"）。
//
// v1.6.0：优先用 keys.json 里第一条启用的密钥；还没迁移的机器回退到 plist 的 --token。
func (m *Manager) receiverToken() (string, error) {
	p := m.receiverPaths()
	if _, err := os.Stat(p.Plist); err != nil {
		return "", fmt.Errorf("接收端还没部署过（找不到 %s）", p.Plist)
	}
	if keys, err := m.LoadVoiceKeys(); err == nil {
		for _, k := range keys {
			if k.Enabled && k.Key != "" {
				return k.Key, nil
			}
		}
	}
	return m.existingReceiverToken(p), nil
}

// ResetVoiceUsage 让接收端清零用量（v1.7.0）。
//
// keyID 为空且 all=true 时清空全部。**必须带管理密钥** —— 普通调用密钥
// （网站手里的那把）不能重置自己的额度，否则额度只是建议。
func (m *Manager) ResetVoiceUsage(ctx context.Context, keyID string, all bool) (map[string]any, error) {
	p := m.receiverPaths()
	admin := m.existingReceiverAdminToken(p)
	if admin == "" {
		return nil, fmt.Errorf("接收端还没有管理密钥（--admin-token）。" +
			"请在应用市场重新部署一次接收端，之后再清零用量")
	}
	body, err := json.Marshal(map[string]any{"key_id": keyID, "all": all})
	if err != nil {
		return nil, err
	}
	code, out, err := m.receiverCall(ctx, http.MethodPost, "/usage/reset", body,
		map[string]string{"X-TtsVoice-Admin": admin, "Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("接收端返回 %d：%s", code, receiverErrorMessage(code, out))
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("接收端响应无法解析：%v", err)
	}
	return doc, nil
}

// VoiceUsage 读接收端的 /usage（各密钥额度/已用 + 全局合计）。
func (m *Manager) VoiceUsage(ctx context.Context) (map[string]any, error) {
	code, body, err := m.receiverCall(ctx, http.MethodGet, "/usage", nil, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, fmt.Errorf("接收端版本过旧（没有 /usage）。" +
			"请在应用市场重新部署「音色样本接收端」，把它升到 v1.6.0")
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("接收端返回 %d：%s", code, receiverErrorMessage(code, body))
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("接收端响应无法解析：%v", err)
	}
	return doc, nil
}

// VoiceKeysView 把 keys.json 与接收端的实时用量合并成面板要用的列表。
//
// 合并在这里做（而不是让前端拼）：额度、剩余、今日字数这些字段必须来自
// 接收端的实际统计，缺一个就会出现"面板显示还有额度、实际已经调不动"。
func (m *Manager) VoiceKeysView(ctx context.Context) (map[string]any, error) {
	keys, err := m.LoadVoiceKeys()
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []VoiceKey{}
	}

	usage, uerr := m.VoiceUsage(ctx)

	byID := map[string]map[string]any{}
	total := map[string]any{}
	if uerr == nil {
		if list, ok := usage["keys"].([]any); ok {
			for _, item := range list {
				mm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if id, _ := mm["id"].(string); id != "" {
					byID[id] = mm
				}
			}
		}
		if t, ok := usage["total"].(map[string]any); ok {
			total = t
		}
	}

	views := make([]VoiceKeyView, 0, len(keys))
	seen := map[string]bool{}
	fromUsage := func(id string) map[string]any { return byID[id] }
	num := func(mm map[string]any, k string) int64 {
		if mm == nil {
			return 0
		}
		switch v := mm[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		}
		return 0
	}

	for _, k := range keys {
		seen[k.ID] = true
		u := fromUsage(k.ID)
		view := VoiceKeyView{
			VoiceKey:       k,
			UsedChars:      num(u, "used_chars"),
			ReservedChars:  num(u, "reserved_chars"),
			RemainingChars: num(u, "remaining_chars"),
			Unlimited:      k.QuotaChars <= 0,
			TodayChars:     num(u, "today_chars"),
			WeekChars:      num(u, "week_chars"),
			Requests:       num(u, "requests"),
			Jobs:           num(u, "jobs"),
			AudioBytes:     num(u, "audio_bytes"),
			FirstUsed:      num(u, "first_used"),
			LastUsed:       num(u, "last_used"),
		}
		if k.QuotaChars > 0 {
			// 剩余必须把"在跑的作业占用的额度"也扣掉：v1.7.0 起额度检查是按
			// 已用+占用算的，这里少扣一次占用，界面就会显示"还有额度"、
			// 提交却被 429 拒 —— 这正是不该出现的那类误导。
			view.RemainingChars = k.QuotaChars - view.UsedChars - view.ReservedChars
			if view.RemainingChars < 0 {
				view.RemainingChars = 0
			}
		}
		if u != nil {
			if days, ok := u["days"].(map[string]any); ok {
				view.Days = map[string]int64{}
				for d, v := range days {
					if f, ok := v.(float64); ok {
						view.Days[d] = int64(f)
					}
				}
			}
		}
		views = append(views, view)
	}

	// 接收端报的、但 keys.json 里没有的条目：可能是 plist 兼容密钥（legacy），
	// 也可能是已经删掉的密钥留下的历史用量（deleted）。两者都要显示出来 ——
	// 前者其实还能调用，后者是"总量对得上"的依据。
	legacy := false
	for _, item := range byID {
		id, _ := item["id"].(string)
		if id == "" || seen[id] {
			continue
		}
		isLegacy, _ := item["legacy"].(bool)
		isDeleted, _ := item["deleted"].(bool)
		if isLegacy {
			legacy = true
		}
		name, _ := item["name"].(string)
		views = append(views, VoiceKeyView{
			VoiceKey: VoiceKey{
				ID: id, Name: name, QuotaChars: 0,
				Enabled: isLegacy, // 兼容密钥其实还能调用
			},
			UsedChars:  num(item, "used_chars"),
			Unlimited:  true,
			TodayChars: num(item, "today_chars"),
			WeekChars:  num(item, "week_chars"),
			Requests:   num(item, "requests"),
			Jobs:       num(item, "jobs"),
			AudioBytes: num(item, "audio_bytes"),
			FirstUsed:  num(item, "first_used"),
			LastUsed:   num(item, "last_used"),
			Deleted:    isDeleted,
			Legacy:     isLegacy,
		})
	}

	out := map[string]any{
		"keys":  views,
		"total": total,
		"path":  m.receiverPaths().Keys,
		// legacy=true 表示这台机器还在用 plist 里的兼容密钥（重新部署后会迁移成可管理的密钥）
		"legacy": legacy,
	}
	if uerr != nil {
		out["usage_error"] = uerr.Error()
	}
	return out, nil
}

// receiverCall 调一次接收端接口，返回状态码与响应体。
func (m *Manager) receiverCall(ctx context.Context, method, path string, body []byte,
	headers map[string]string) (int, []byte, error) {
	token, err := m.receiverToken()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, m.receiverBaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("X-TtsVoice-Token", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("接收端不可达（%s）：%v。请先在服务管理里确认它在运行",
			m.receiverBaseURL(), err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

// receiverErrorMessage 从接收端的错误体里取出人话。
// 它返回 {"ok":false,"error":"…"}（/jobs 口径）或 {"ok":false,"msg":"…"}。
func receiverErrorMessage(code int, body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"error", "msg", "message"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = http.StatusText(code)
	}
	return text
}

// VoiceSources 列出各来源（转发接收端的 /voice/sources）。
func (m *Manager) VoiceSources(ctx context.Context) ([]VoiceSource, error) {
	code, body, err := m.receiverCall(ctx, http.MethodGet, "/voice/sources", nil, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, fmt.Errorf("接收端版本过旧（没有 /voice/sources）。" +
			"请在应用市场重新部署「音色样本接收端」，把它升到 v1.5.0")
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("接收端返回 %d：%s", code, receiverErrorMessage(code, body))
	}
	var doc struct {
		Dir     string        `json:"dir"`
		Sources []VoiceSource `json:"sources"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("接收端响应无法解析：%v", err)
	}
	// 标注哪一条是面板内置的默认音色（安装接收端时写的，带 builtin-voice.json 标记）
	marker := filepath.Join(m.receiverPaths().Samples, defaultVoiceSource, builtinVoiceMarker)
	if _, err := os.Stat(marker); err == nil {
		for i := range doc.Sources {
			// 只标**内置的那一份**：v1.4.0 残留的根目录 ref.wav 也记在 default
			// 名下（legacy=true），它跟内置音色是两回事，不能也标成"内置"。
			if doc.Sources[i].Source == defaultVoiceSource && !doc.Sources[i].Legacy {
				doc.Sources[i].Builtin = true
			}
		}
	}
	return doc.Sources, nil
}

// VoiceSourceUpload 上传/替换一个来源的样本，返回接收端的响应。
//
// 校验与归一化都在接收端做（只有一处实现），这里只负责带上来源与文件名。
func (m *Manager) VoiceSourceUpload(ctx context.Context, source, filename, contentType string,
	data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("上传的文件是空的")
	}
	headers := map[string]string{
		"X-TtsVoice-Source": SanitizeVoiceSource(source),
		"Content-Type":      contentType,
	}
	if filename != "" {
		headers["X-TtsVoice-Name"] = filename
	}
	code, body, err := m.receiverCall(ctx, http.MethodPost, "/voice", data, headers)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if code != http.StatusOK {
		return nil, fmt.Errorf("接收端拒绝了这次上传（%d）：%s",
			code, receiverErrorMessage(code, body))
	}
	if doc == nil {
		return nil, fmt.Errorf("接收端响应无法解析")
	}
	return doc, nil
}

// VoiceSourceDelete 删除一个来源（转发接收端的 DELETE /voice/sources/<source>）。
func (m *Manager) VoiceSourceDelete(ctx context.Context, source string) error {
	src := SanitizeVoiceSource(source)
	code, body, err := m.receiverCall(ctx, http.MethodDelete,
		"/voice/sources/"+url.PathEscape(src), nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("接收端返回 %d：%s", code, receiverErrorMessage(code, body))
	}
	return nil
}

// receiverPlist 生成系统级 LaunchDaemon 定义。
func receiverPlist(p receiverPaths, user, host, adminToken string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <!-- 以真实用户运行：音色样本要落到该用户的家目录下 -->
    <key>UserName</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/bin/python3</string>
        <string>%s</string>
        <string>--dir</string>
        <string>%s</string>
        <!-- 作业队列目录也显式给绝对路径：launchd 下 HOME 不一定是真实用户家目录，
             而 ~ 的展开依赖 HOME —— --dir 一直是显式传的，这里保持一致。
             v1.3.0 的 /jobs/* 会把状态与分块落在这里（重启后据此恢复）。 -->
        <string>--jobs-dir</string>
        <string>%s</string>
        <!-- 监听地址：0.0.0.0 = 对局域网开放（网站可能在别的机器上）；
             127.0.0.1 = 只有本机能连（网站与接收端同一台机器时的推荐值） -->
        <string>--host</string>
        <string>%s</string>
        <string>--port</string>
        <string>%d</string>
        <!-- v1.6.0：密钥与每密钥额度都在这个文件里（面板写、接收端按 mtime 热加载）。
             所以 plist 里**不再放密钥**：加一个网站的密钥不需要重启正在跑合成的服务。
             （老 plist 里的 --token 仍被接收端接受，重新部署时会被迁移进这个文件。） -->
        <string>--keys-file</string>
        <string>%s</string>
        <!-- v1.7.0：管理密钥。只有面板持有它（网站插件看不到），用于"清零用量"
             这类管理操作 —— 如果能用普通调用密钥清零自己的额度，额度就形同虚设。 -->
        <string>--admin-token</string>
        <string>%s</string>
        <!-- 上游 Qwen 服务只监听 127.0.0.1，由本代理对外提供鉴权 -->
        <string>--upstream</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, receiverLabel, user, p.Script, p.Samples, p.Jobs, host, receiverPort, p.Keys, adminToken,
		qwenUpstream, p.Dir, p.OutLog, p.ErrLog)
}

// waitJSONBool 轮询某个 URL，直到 JSON 响应里指定字段为 true。
//
// 为什么必须解析 JSON 而不是做子串匹配：
// 我第一版写成找子串 `"auth":true`，而 Python 的 json.dumps 默认带空格，
// 实际返回的是 `"auth": true` —— 于是**服务明明是好的，却被判成失败**，
// 还带着一句"请查看日志"的误导提示。判 JSON 字段就要按 JSON 解，
// 不能对格式化后的文本做精确匹配。
//
// 也不能只看 HTTP 200：接收端在鉴权没生效时同样返回 200（只是 auth:false）。
// 没鉴权的接收端等于一个**文件投放点**，必须确认字段为 true 才算过。
func waitJSONBoolValue(ctx context.Context, url, field string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if out, err := runCurlCtx(ctx, url, 4); err == nil {
			var m map[string]any
			if json.Unmarshal([]byte(out), &m) == nil {
				if v, ok := m[field].(bool); ok && v == want {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}
