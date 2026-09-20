#!/usr/bin/env python3
"""按版本生成发布说明（写进 manifest.json 的 notes，面板"本次更新内容"读它）。

范围 = **上一个 version bump 提交 → 本版 version bump 提交**（发布点）。
为什么不取到 HEAD：bump 之后的提交属于下一版；算进来的话，已发布版本的说明
会随任何新提交漂移，线上清单与生成物立刻对不上（门禁会红）。正常发布时
HEAD 就是本版 bump 提交，两者等价。

输出一行：`v<版本> · 要点1；要点2…`（与面板现有 Markdown 渲染兼容）。
最多 6 条、总长 ≤ 600 字符；超出截断并补 `…详见 git 历史`。

背景：从 1.3.4 起 manifest.notes 一直复用静态 RELEASE_NOTES.md，版本到了
1.6.5 说明还停在 v1.3.4（只有文件内容变过才看得到）。所以这里改成生成式。

用法：
    python3 tools/gen-release-notes.py                 # 写到 dist/release/NOTES.md
    python3 tools/gen-release-notes.py --out /tmp/n.md
    python3 tools/gen-release-notes.py --print         # 只打印
"""
import argparse
import os
import re
import subprocess
import sys

MAX_ITEMS = 6
MAX_CHARS = 600
TRUNC_SUFFIX = "…详见 git 历史"
EMPTY_BODY = "常规维护更新"

# 这些类型的提交对用户没有可见变化，直接当噪音丢掉（feat/fix/docs/... 保留）。
DROP_TYPES = {"chore", "ci", "test", "style", "build", "version", "release"}
# 无 conventional-commit 前缀时的噪音标题。
NOISE_SUBJECTS = re.compile(r"^(version\b|release\b|wip\b|merge\b)", re.I)
SUBJECT_RE = re.compile(
    r"^(?P<type>[A-Za-z]+)(?:\((?P<scope>[^)]*)\))?!?:\s*(?P<desc>.*)$"
)
TAG_RE = re.compile(r"^\[[^\]]*\]\s*")


def repo_root():
    out = subprocess.run(
        ["git", "rev-parse", "--show-toplevel"], capture_output=True, text=True
    )
    if out.returncode != 0:
        sys.exit("!! 不在 git 仓库里，无法按提交生成发布说明")
    return out.stdout.strip()


def git(*args, cwd=None):
    out = subprocess.run(["git", *args], capture_output=True, text=True, cwd=cwd)
    if out.returncode != 0:
        sys.exit(f"!! git {' '.join(args)} 失败：{out.stderr.strip()}")
    return out.stdout


def read_version(root):
    path = os.path.join(root, "internal/version/version.go")
    with open(path, encoding="utf-8") as f:
        for line in f:
            m = re.match(r'^var Version = "([0-9][0-9.]*)"', line)
            if m:
                return m.group(1)
    sys.exit("!! 读不到 internal/version/version.go 里的 var Version")


def version_at(root, commit):
    out = git("show", f"{commit}:internal/version/version.go", cwd=root)
    m = re.search(r'^var Version = "([0-9][0-9.]*)"', out, re.M)
    return m.group(1) if m else None


def bump_range(root, version):
    """返回 (prev_bump, bump) 两个提交号；bump = 把版本设成 version 的提交。"""
    commits = git(
        "log", "--format=%H", "-G", r'^var Version = "[0-9]',
        "--", "internal/version/version.go", cwd=root,
    ).split()
    for i, c in enumerate(commits):
        if version_at(root, c) == version:
            prev = commits[i + 1] if i + 1 < len(commits) else None
            return prev, c
    return None, None


def clean_subject(subject):
    """去掉 feat(...)/fix(...) 之类前缀，返回 (是否噪音, 要点文本)。"""
    subject = subject.strip()
    m = SUBJECT_RE.match(subject)
    if m:
        if m.group("type").lower() in DROP_TYPES:
            return True, ""
        desc = m.group("desc").strip()
    else:
        if NOISE_SUBJECTS.match(subject):
            return True, ""
        desc = TAG_RE.sub("", subject).strip()
    return (desc == ""), desc


def collect(root, range_spec):
    subjects = git("log", "--no-merges", "--format=%s", range_spec, cwd=root).splitlines()
    items = []
    for s in subjects:
        noise, desc = clean_subject(s)
        if noise or desc in items:
            continue
        items.append(desc)
    return items


def compose(version, items):
    if not items:
        items = [EMPTY_BODY]
    total = len(items)
    if total > MAX_ITEMS:
        items = items[:MAX_ITEMS]
        items[-1] = f"{items[-1]}（等 {total} 项）"
    prefix = f"v{version} · "
    body = "；".join(items)
    if len(prefix) + len(body) > MAX_CHARS:
        keep = MAX_CHARS - len(prefix) - len(TRUNC_SUFFIX)
        body = body[:keep].rstrip("；、,， ")
        body += TRUNC_SUFFIX
    return prefix + body


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="dist/release/NOTES.md", help="输出路径")
    ap.add_argument("--version", default="", help="覆盖版本号（默认读 version.go）")
    ap.add_argument("--range", default="", help="覆盖提交范围（测试用）")
    ap.add_argument("--print", action="store_true", help="只打印，不写文件")
    args = ap.parse_args()

    root = repo_root()
    version = args.version or read_version(root)
    if args.range:
        range_spec = args.range
    else:
        prev, bump = bump_range(root, version)
        if not bump:
            sys.exit(f"!! 找不到把版本设为 {version} 的 version bump 提交")
        if prev:
            range_spec = f"{prev}..{bump}"
        else:
            range_spec = f"{bump}^..{bump}" if git("rev-parse", f"{bump}^", cwd=root).strip() else bump

    notes = compose(version, collect(root, range_spec))
    if args.print:
        print(notes)
        return 0
    out = args.out if os.path.isabs(args.out) else os.path.join(root, args.out)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w", encoding="utf-8") as f:
        f.write(notes + "\n")
    print(f"==> 已生成发布说明 {out}（{len(notes)} 字，范围 {range_spec}）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
