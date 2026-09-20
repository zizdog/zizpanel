package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zizdog/zizvideo/internal/config"
	"github.com/zizdog/zizvideo/internal/media"
	"github.com/zizdog/zizvideo/internal/storage"
)

// rootsOut is the machine-readable one-line answer of every roots subcommand.
type rootsOut struct {
	Action     string   `json:"action"`
	OK         bool     `json:"ok"`
	Path       string   `json:"path,omitempty"`
	Roots      []string `json:"roots"`
	ConfigPath string   `json:"config_path"`
	InUse      []string `json:"in_use,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// runRoots handles `zizvideo roots list|add|remove`: it reads and writes the
// same config.json the server uses and prints one JSON line per invocation.
func runRoots(args []string) error {
	fs := flag.NewFlagSet("roots", flag.ContinueOnError)
	configPath := fs.String("config", "", "配置文件路径 (JSON)")
	if err := fs.Parse(stripFlag(args, "--config", configPath)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("用法: zizvideo roots list|add <path>|remove <path> [--config <file>]")
	}
	path := *configPath
	if path == "" {
		path = os.Getenv("ZV_CONFIG")
	}
	if path == "" {
		return errors.New("roots 子命令必须带 --config 或 ZV_CONFIG")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	verb := rest[0]

	current, err := config.RootsFromFile(path)
	if err != nil {
		return err
	}
	roots := config.NewRoots(path, current)

	switch verb {
	case "list":
		return printRoots(rootsOut{Action: "list", OK: true, Roots: current, ConfigPath: path})
	case "add":
		if len(rest) < 2 {
			return errors.New("用法: zizvideo roots add <绝对路径>")
		}
		clean, verr := config.ValidateAllowRoot(rest[1])
		if verr != nil {
			return fmt.Errorf("拒绝添加: %w", verr)
		}
		if covering := media.CoveringRoot(current, clean); covering != "" {
			return fmt.Errorf("拒绝添加: 已在允许根内 %s", covering)
		}
		next, aerr := roots.Add(clean)
		if aerr != nil {
			return aerr
		}
		return printRoots(rootsOut{Action: "add", OK: true, Path: clean,
			Roots: next, ConfigPath: path})
	case "remove":
		if len(rest) < 2 {
			return errors.New("用法: zizvideo roots remove <绝对路径>")
		}
		if !filepath.IsAbs(rest[1]) {
			return errors.New("必须是绝对路径")
		}
		clean := config.ResolvePath(rest[1])
		stored := false
		for _, existing := range current {
			if existing == clean || existing == filepath.Clean(rest[1]) {
				clean = existing
				stored = true
				break
			}
		}
		if !stored {
			return errors.New("该路径不在允许根里")
		}
		if used := librariesUnderRoot(path, clean); len(used) > 0 {
			return printRoots(rootsOut{Action: "remove", OK: false, Path: clean,
				Roots: current, ConfigPath: path, InUse: used,
				Error: "该允许根正被媒体库使用，未删除"})
		}
		next, rerr := roots.Remove(clean)
		if rerr != nil {
			return rerr
		}
		return printRoots(rootsOut{Action: "remove", OK: true, Path: clean,
			Roots: next, ConfigPath: path})
	default:
		return fmt.Errorf("未知子命令 %q（list|add|remove）", verb)
	}
}

// stripFlag pulls --config (with either spelling) out of args so it may appear
// before or after the verb: flag.Parse stops at the first positional argument.
func stripFlag(args []string, name string, dst *string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == name && i+1 < len(args):
			*dst = args[i+1]
			i++
		case strings.HasPrefix(arg, name+"="):
			*dst = strings.TrimPrefix(arg, name+"=")
		default:
			out = append(out, arg)
		}
	}
	return out
}

func printRoots(out rootsOut) error {
	if out.Roots == nil {
		out.Roots = []string{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// librariesUnderRoot opens the DB next to the config just to list libraries;
// it never mutates anything and returns nil when the DB cannot be read.
func librariesUnderRoot(configPath, root string) []string {
	cfg, _, err := config.Load(configPath)
	if err != nil {
		return nil
	}
	db, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		return nil
	}
	defer db.Close()
	libs, err := db.ListLibraries()
	if err != nil {
		return nil
	}
	alias := ""
	if a, aerr := filepath.EvalSymlinks(root); aerr == nil {
		alias = a
	}
	names := []string{}
	for _, l := range libs {
		real := l.RootPath
		if r, rerr := filepath.EvalSymlinks(l.RootPath); rerr == nil {
			real = r
		}
		if media.Within(l.RootPath, root) || media.Within(real, root) ||
			(alias != "" && (media.Within(l.RootPath, alias) || media.Within(real, alias))) {
			names = append(names, l.Name)
		}
	}
	return names
}
