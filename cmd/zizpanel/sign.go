package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zizdog/zizpanel/internal/upgrade"
)

// cmdSignManifest 给发布清单签名（发布流程使用，面板运行时不会调用）。
//
// 用法：
//
//	zizpanel sign-manifest --key <私钥文件> --in manifest.json --out manifest.json.sig
//	zizpanel sign-manifest --gen-key <私钥文件>          # 生成新密钥对并打印公钥
//
// 为什么把密钥生成也放在这里：生成之后必须立刻把**公钥**填进
// internal/upgrade.PubKeyHex（或构建时注入），否则面板会 fail closed
// 拒绝一切网络升级。让生成公钥和打印"下一步该做什么"发生在同一个命令里，
// 能显著降低"生成了密钥却忘了填公钥"的概率。
func cmdSignManifest(args []string) error {
	fs := flag.NewFlagSet("sign-manifest", flag.ContinueOnError)
	keyPath := fs.String("key", "", "Ed25519 私钥文件路径")
	in := fs.String("in", "manifest.json", "要签名的清单文件")
	out := fs.String("out", "manifest.json.sig", "签名输出路径")
	genKey := fs.String("gen-key", "", "生成新密钥对到该路径，并打印公钥")
	pubFromKey := fs.String("pub-from-key", "", "从私钥推导公钥并打印（构建时注入用）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 构建流程用：从私钥推出公钥，写进 -ldflags。
	// 放在这里而不是让 Makefile 自己算，是为了保证"签名用的私钥"
	// 与"面板内嵌的公钥"永远配对 —— 这两者不一致的话，
	// 表现是升级时验签失败，而那种错误极难从表面看出来。
	if *pubFromKey != "" {
		priv, err := upgrade.LoadPrivateKey(*pubFromKey)
		if err != nil {
			return fmt.Errorf("读取私钥失败: %w", err)
		}
		fmt.Println(hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
		return nil
	}

	if *genKey != "" {
		if _, err := os.Stat(*genKey); err == nil {
			return fmt.Errorf("%s 已存在，拒绝覆盖（私钥丢了就再也签不出可被现有面板接受的升级包）", *genKey)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*genKey), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(*genKey, []byte(hex.EncodeToString(priv)), 0o600); err != nil {
			return err
		}
		pubHex := hex.EncodeToString(pub)
		fmt.Printf("私钥已写入：%s（权限 0600，务必离线备份，丢失后无法再发布可升级的版本）\n", *genKey)
		fmt.Printf("公钥（需要嵌入面板）：\n%s\n", pubHex)
		fmt.Println()
		fmt.Println("下一步：把上面这串公钥写入 internal/upgrade/manifest.go 的 PubKeyHex，")
		fmt.Println("或在构建时注入：-ldflags \"-X github.com/zizdog/zizpanel/internal/upgrade.PubKeyHex=<公钥>\"")
		return nil
	}

	if *keyPath == "" {
		return fmt.Errorf("必须指定 --key（私钥文件）或 --gen-key")
	}
	priv, err := upgrade.LoadPrivateKey(*keyPath)
	if err != nil {
		return fmt.Errorf("读取私钥失败: %w", err)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("读取清单失败: %w", err)
	}
	// 先解析一遍：签一份自己都解析不了的清单没有意义，
	// 而且会把错误推迟到用户升级时才暴露。
	if _, err := upgrade.ParseManifest(data); err != nil {
		return fmt.Errorf("清单内容不合法，拒绝签名: %w", err)
	}
	sig := upgrade.SignManifest(priv, data)
	if err := os.WriteFile(*out, sig, 0o644); err != nil {
		return err
	}
	fmt.Printf("已签名：%s → %s\n", *in, *out)
	fmt.Printf("公钥指纹：%s\n", hex.EncodeToString(priv.Public().(ed25519.PublicKey))[:16])
	return nil
}
