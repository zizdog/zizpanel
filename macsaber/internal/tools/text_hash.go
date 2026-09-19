package tools

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/macsaber/internal/tool"
)

// textHash 是**同步工具**打样：纯标准库算文件/文本哈希，不调外部命令。
type textHash struct{}

func init() { Add(textHash{}) }

func (textHash) Meta() tool.Meta {
	return tool.Meta{
		ID: "text.hash", Name: "哈希计算", Category: "text", Icon: "hash",
		Summary:   "算文本或文件的 sha256 / sha1 / md5，纯 Go 实现。",
		Available: true,
		Params: []tool.Param{
			{Name: "algo", Label: "算法", Type: tool.TypeSelect, Required: true, Default: "sha256",
				Options: []tool.Option{
					{Value: "sha256", Label: "SHA-256"},
					{Value: "sha1", Label: "SHA-1"},
					{Value: "md5", Label: "MD5"},
				}},
			{Name: "text", Label: "文本", Type: tool.TypeTextarea, Multiline: true,
				Placeholder: "要计算哈希的文本", Help: "与文件二选一，同时填则都算。"},
			{Name: "file", Label: "文件", Type: tool.TypePath,
				Help: "只能读允许的读根内的文件。"},
		},
	}
}

func (textHash) Run(ctx context.Context, c *tool.Ctx, in tool.Input) (*tool.Result, error) {
	algo := in.Str("algo")
	newHash, err := hasher(algo)
	if err != nil {
		return nil, err
	}
	text := in.Str("text")
	file := in.Path("file")
	if strings.TrimSpace(text) == "" && file == "" {
		return nil, errors.New("请填文本或选一个文件（至少一个）")
	}
	data := map[string]any{"algo": algo}

	if file != "" {
		h := newHash()
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("打开文件失败：%v", err)
		}
		defer func() { _ = f.Close() }()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, fmt.Errorf("读取文件失败：%v", err)
		}
		sum := hex.EncodeToString(h.Sum(nil))
		data["file"] = map[string]any{"path": file, "name": filepath.Base(file), "size": n, "hash": sum}
		c.Logf("文件 %s → %s", filepath.Base(file), sum)
	}
	if strings.TrimSpace(text) != "" {
		h := newHash()
		h.Write([]byte(text))
		sum := hex.EncodeToString(h.Sum(nil))
		data["text"] = map[string]any{"chars": len([]rune(text)), "bytes": len(text), "hash": sum}
		c.Logf("文本 %d 字符 → %s", len([]rune(text)), sum)
	}
	return &tool.Result{OK: true, Msg: strings.ToUpper(algo) + " 计算完成", Data: data}, nil
}

func hasher(algo string) (func() hash.Hash, error) {
	switch strings.ToLower(algo) {
	case "sha256":
		return sha256.New, nil
	case "sha1":
		return sha1.New, nil
	case "md5":
		return md5.New, nil
	}
	return nil, fmt.Errorf("不支持的算法: %s", algo)
}
