#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# SPDX-License-Identifier: MIT
"""wpsdav-sync: asymmetric one-way-delete sync for WPS/kDrive over rclone.

Rules:
  local new/changed      -> upload
  local deleted          -> delete on remote
  remote new/changed     -> download
  remote deleted         -> keep local, do not re-upload

Usage:
  wpsdav-sync.py --remote wps:backup/MyDocs --exclude cache --dry-run
  wpsdav-sync.py --local ~/Documents/MyDocs --remote wps:backup/MyDocs \
      --exclude-dir node_modules --exclude-dir build --gitignore

--local defaults to the current directory, and --remote chooses the cloud
target, so the same command can be run from several directories (each
(local, remote) pair gets its own state file and lock).

--exclude may be repeated to drop path prefixes; --exclude-dir drops
directories by name at any depth; --gitignore additionally skips everything
ignored by Git, including .gitignore files in subdirectories and nested
repositories (the root itself does not have to be a repository).

No persistent content cache is used; only a small JSON state file is kept (by
default under ~/.local/state/wpsdav-sync/).

This is a reference helper, not part of the wpsdav server.  See examples/
README.md for the behavior matrix and limitations.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import signal
import stat
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

try:
    import fcntl
except ImportError:  # Windows: no advisory lock, see acquire_lock()
    fcntl = None

VERSION = 1
DOTFILE_PATTERN = ".*"
TEMPFILE_PATTERN = "~$*"
# WPS rejects these characters in any path component. "/" is omitted because
# it is the path separator on every platform we support.
WPS_ILLEGAL_CHARS = set('\\:*?"<>|')


def eprint(*args, **kwargs):
    print(*args, file=sys.stderr, flush=True)


def human_bytes(n: float) -> str:
    if n is None:
        return "?"
    n = float(n)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if abs(n) < 1024.0 or unit == "TiB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{int(n)} B"
        n /= 1024.0
    return f"{n:.1f} TiB"


def human_eta(sec) -> str:
    if sec is None:
        return "-"
    try:
        sec = int(sec)
    except (TypeError, ValueError):
        return "-"
    if sec < 60:
        return f"{sec}s"
    m, s = divmod(sec, 60)
    if m < 60:
        return f"{m}m{s:02d}s"
    h, m = divmod(m, 60)
    return f"{h}h{m:02d}m"


class Fatal(Exception):
    pass


@dataclass
class Action:
    kind: str
    reason: str = ""
    conflict: bool = False


@dataclass
class Config:
    local: Path
    remote: str
    excludes: list[str]
    exclude_dirs: list[str]
    gitignore: bool
    state_dir: Path
    rclone: str
    transfers: int
    checkers: int
    buffer_size: str
    dry_run: bool
    verbose: bool
    force: bool
    max_delete_pct: int
    stats_interval: str


@dataclass
class Ignored:
    """Paths ignored by Git, relative to the sync root."""

    dirs: set[str]
    files: set[str]

    def matches(self, rel: str) -> bool:
        if rel in self.files:
            return True
        parts = rel.split("/")
        for i in range(1, len(parts) + 1):
            if "/".join(parts[:i]) in self.dirs:
                return True
        return False


def matches_user_exclude(rel: str, excludes) -> bool:
    for ex in excludes:
        ex = ex.replace("\\", "/").strip("/")
        if ex and (rel == ex or rel.startswith(ex + "/")):
            return True
    return False


def is_excluded_rel(rel: str, excludes, exclude_dirs=(), ignored=None) -> bool:
    parts = rel.split("/")
    if any(p.startswith(".") for p in parts):
        return True
    if parts and parts[-1].startswith("~$"):
        return True
    # Only parent components count: a file may legitimately be named like an
    # excluded directory without being under one.
    if exclude_dirs and any(p in exclude_dirs for p in parts[:-1]):
        return True
    if ignored is not None and ignored.matches(rel):
        return True
    return matches_user_exclude(rel, excludes)


def _add_ignored(stdout: bytes, prefix: str, dirs: set, files: set):
    for raw in stdout.split(b"\0"):
        if not raw:
            continue
        rel = raw.decode("utf-8", "surrogateescape").replace(os.sep, "/")
        if prefix:
            rel = f"{prefix}/{rel}"
        if rel.endswith("/"):
            dirs.add(rel.rstrip("/"))
        else:
            files.add(rel)


def _git_ls_ignored(args, cwd, env, prefix, dirs, files, where):
    p = subprocess.run(args, cwd=cwd, env=env, capture_output=True)
    if p.returncode != 0:
        err = p.stderr.decode("utf-8", "replace").strip()
        raise Fatal(f"读取 git 忽略列表失败 ({where}): {err}")
    _add_ignored(p.stdout, prefix, dirs, files)


def _inside_worktree(root: Path) -> bool:
    try:
        p = subprocess.run(
            ["git", "-C", str(root), "rev-parse", "--is-inside-work-tree"],
            text=True, capture_output=True)
    except FileNotFoundError:
        raise Fatal("--gitignore 需要安装 git")
    return p.returncode == 0 and p.stdout.strip() == "true"


def _dir_ignored(rel: str, dirs) -> bool:
    parts = rel.split("/")
    for i in range(1, len(parts) + 1):
        if "/".join(parts[:i]) in dirs:
            return True
    return False


def _find_nested_repos(root: Path, ignored_dirs) -> list[str]:
    """Return repo roots below root (relative paths, no trailing slash).

    A directory holding a .git file or directory is a repo boundary: the
    enclosing repo does not descend into it, so its own rules must be read
    separately.  Directories hidden by the enclosing repo, and dot directories
    (never synced), are pruned.
    """
    out = []
    for dirpath, dirnames, _ in os.walk(root, topdown=True, followlinks=False):
        rel_dir = os.path.relpath(dirpath, root).replace(os.sep, "/")
        if rel_dir == ".":
            rel_dir = ""
        if rel_dir and _dir_ignored(rel_dir, ignored_dirs):
            dirnames[:] = []
            continue
        dirnames[:] = [d for d in dirnames
                       if d != ".git" and not d.startswith(".")]
        if rel_dir and os.path.lexists(os.path.join(dirpath, ".git")):
            out.append(rel_dir)
    return out


def collect_gitignored(root: Path, enabled: bool) -> Ignored:
    """Return the paths Git would ignore below root.

    The repo containing root is asked first (tracked files are therefore never
    ignored, and .git/info/exclude applies).  If root is not in a repo, a
    temporary bare index is pointed at root so plain .gitignore files are still
    honoured.  Neither call crosses a nested repo boundary, so every nested
    repo found by the walk is queried with its own rules.
    """
    if not enabled:
        return Ignored(set(), set())
    if not shutil_which("git"):
        raise Fatal("--gitignore 需要安装 git")
    dirs, files = set(), set()
    ls_args = ["ls-files", "--others", "--ignored", "--exclude-standard",
               "--directory", "-z"]

    if _inside_worktree(root):
        _git_ls_ignored(["git", "-C", str(root)] + ls_args, None, None,
                        "", dirs, files, str(root))
    else:
        # No repository: use a throwaway index and let Git evaluate every
        # .gitignore in the tree, including ones without a .git next to them.
        with tempfile.TemporaryDirectory(prefix="wpsdav-sync-git-") as td:
            init = subprocess.run(["git", "init", "-q", "--bare", td],
                                  capture_output=True)
            if init.returncode != 0:
                err = init.stderr.decode("utf-8", "replace").strip()
                raise Fatal(f"创建临时 Git 索引失败: {err}")
            env = os.environ.copy()
            env["GIT_DIR"] = td
            env["GIT_WORK_TREE"] = str(root)
            _git_ls_ignored(["git"] + ls_args, str(root), env, "",
                            dirs, files, f"{root}（无仓库）")

    for repo_rel in _find_nested_repos(root, dirs):
        repo = root / repo_rel
        _git_ls_ignored(["git", "-C", str(repo)] + ls_args, None, None,
                        repo_rel, dirs, files, str(repo))
    return Ignored(dirs, files)


def make_state_paths(cfg: Config):
    cfg.state_dir.mkdir(parents=True, exist_ok=True)
    key = hashlib.sha256(
        (str(cfg.local.resolve()) + "\x00" + cfg.remote).encode("utf-8")
    ).hexdigest()[:16]
    return (
        cfg.state_dir / f"state-{key}.json",
        cfg.state_dir / f"lock-{key}",
        cfg.state_dir / f"last-run-{key}.log",
        key,
    )


def acquire_lock(path: Path):
    fd = os.open(path, os.O_CREAT | os.O_RDWR, 0o600)
    if fcntl is None:
        return fd
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        raise Fatal("已有另一个同步进程在运行")
    return fd


def load_state(path: Path, cfg: Config, ignored: Ignored):
    if not path.exists():
        return {}, True
    try:
        with open(path, "r", encoding="utf-8") as f:
            doc = json.load(f)
    except (OSError, json.JSONDecodeError) as exc:
        raise Fatal(
            f"状态文件损坏: {path}\n{exc}\n"
            f"请先备份/移走该文件后重跑（会按首次同步处理）"
        )
    if doc.get("version") != VERSION:
        raise Fatal(f"状态文件版本不兼容: {path}")
    if doc.get("local") != str(cfg.local) or doc.get("remote") != cfg.remote:
        raise Fatal(f"状态文件与当前路径不匹配: {path}")
    files = {}
    for rel, ent in (doc.get("files") or {}).items():
        if is_excluded_rel(rel, cfg.excludes, cfg.exclude_dirs, ignored):
            continue
        l = ent.get("l")
        r = ent.get("r")
        if l is not None:
            l = [int(l[0]), int(l[1])]
        if r is not None:
            r = int(r)
        if l is None and r is None:
            continue
        files[rel] = {"l": l, "r": r}
    return files, False


def write_state(path: Path, cfg: Config, files: dict):
    doc = {
        "version": VERSION,
        "local": str(cfg.local),
        "remote": cfg.remote,
        "saved_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "files": files,
    }
    tmp = path.with_suffix(path.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, separators=(",", ":"))
        f.flush()
        os.fsync(f.fileno())
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def scan_local(root: Path, excludes, exclude_dirs, ignored):
    """Return files, skipped counts/prefixes, and examples for bad names."""
    if not root.is_dir():
        raise Fatal(f"本地目录不存在或不是目录: {root}")
    files = {}
    skipped = {"dot_or_temp": 0, "symlink": 0, "non_regular": 0,
               "excluded_dir": 0, "exclude_dir_name": 0, "vcs_ignored": 0,
               "bad_name": 0, "illegal_name": 0, "empty": 0}
    skip_prefixes = set()
    bad_examples = []
    illegal_examples = []
    empty_examples = []

    def bad_reason(rel: str):
        try:
            rel.encode("utf-8")
        except UnicodeEncodeError:
            return "非 UTF-8 文件名"
        if any(ord(c) < 32 or ord(c) == 127 for c in rel):
            return "含控制字符"
        return None

    def illegal_reason(rel: str):
        for part in rel.split("/"):
            if part != part.rstrip(" ."):
                return "名字末尾有空格或点"
            if any(ch in WPS_ILLEGAL_CHARS for ch in part):
                return "含 WPS 非法字符"
        return None

    def onerror(exc):
        raise Fatal(f"读取本地目录失败: {exc}")

    for dirpath, dirnames, filenames in os.walk(root, topdown=True,
                                               followlinks=False, onerror=onerror):
        rel_dir = os.path.relpath(dirpath, root).replace(os.sep, "/")
        if rel_dir == ".":
            rel_dir = ""
        keep = []
        for d in dirnames:
            rel = f"{rel_dir}/{d}" if rel_dir else d
            if matches_user_exclude(rel, excludes):
                skipped["excluded_dir"] += 1
                continue
            if exclude_dirs and d in exclude_dirs:
                skipped["exclude_dir_name"] += 1
                continue
            if ignored.matches(rel):
                skipped["vcs_ignored"] += 1
                continue
            if any(p.startswith(".") for p in rel.split("/")) or d.startswith("~$"):
                skipped["dot_or_temp"] += 1
                continue
            reason = bad_reason(rel)
            if reason:
                skipped["bad_name"] += 1
                skip_prefixes.add(rel)
                if len(bad_examples) < 10:
                    bad_examples.append((reason, rel))
                continue
            reason = illegal_reason(rel)
            if reason:
                skipped["illegal_name"] += 1
                skip_prefixes.add(rel)
                if len(illegal_examples) < 10:
                    illegal_examples.append((reason, rel))
                continue
            full = os.path.join(dirpath, d)
            try:
                st = os.lstat(full)
            except OSError as exc:
                raise Fatal(f"无法读取 {full}: {exc}")
            if stat.S_ISLNK(st.st_mode):
                skipped["symlink"] += 1
                continue
            keep.append(d)
        dirnames[:] = keep

        for name in filenames:
            rel = f"{rel_dir}/{name}" if rel_dir else name
            if matches_user_exclude(rel, excludes):
                skipped["excluded_dir"] += 1
                continue
            if ignored.matches(rel):
                skipped["vcs_ignored"] += 1
                continue
            if any(p.startswith(".") for p in rel.split("/")) or name.startswith("~$"):
                skipped["dot_or_temp"] += 1
                continue
            reason = bad_reason(rel)
            if reason:
                skipped["bad_name"] += 1
                skip_prefixes.add(rel)
                if len(bad_examples) < 10:
                    bad_examples.append((reason, rel))
                continue
            reason = illegal_reason(rel)
            if reason:
                skipped["illegal_name"] += 1
                skip_prefixes.add(rel)
                if len(illegal_examples) < 10:
                    illegal_examples.append((reason, rel))
                continue
            full = os.path.join(dirpath, name)
            try:
                st = os.lstat(full)
            except OSError as exc:
                raise Fatal(f"无法读取 {full}: {exc}")
            if stat.S_ISLNK(st.st_mode):
                skipped["symlink"] += 1
                continue
            if not stat.S_ISREG(st.st_mode):
                skipped["non_regular"] += 1
                continue
            if st.st_size == 0:
                skipped["empty"] += 1
                skip_prefixes.add(rel)
                if len(empty_examples) < 10:
                    empty_examples.append(rel)
                continue
            files[rel] = (int(st.st_size), int(st.st_mtime_ns))
    return (files, skipped, skip_prefixes, bad_examples,
            illegal_examples, empty_examples)


def base_rclone_cfg(cfg: Config):
    return [
        "--retries", "3",
        "--retries-sleep", "5s",
        "--low-level-retries", "10",
        "--contimeout", "30s",
        "--timeout", "30m",
    ]


def is_not_found(text: str) -> bool:
    low = text.lower()
    return any(s in low for s in (
        "directory not found", "object not found", "not found", "doesn't exist",
    ))


def list_remote(cfg: Config, allow_missing: bool, ignored: Ignored):
    exclude_args = []
    for ex in cfg.excludes:
        p = ex.strip("/")
        if p:
            exclude_args += ["--exclude", f"/{p}", "--exclude", f"/{p}/**"]
    for name in cfg.exclude_dirs:
        # Let rclone pre-filter; names with glob metacharacters are left to
        # the exact Python-side check so they cannot hide unrelated paths.
        if any(ch in name for ch in "*?[]{}"):
            continue
        exclude_args += ["--exclude", f"**/{name}/**"]
    cmd = [cfg.rclone, "lsjson", cfg.remote,
           "--recursive", "--files-only", "--no-modtime", "--no-mimetype",
           "--checkers", str(cfg.checkers),
           "--exclude", DOTFILE_PATTERN,
           "--exclude", TEMPFILE_PATTERN] + exclude_args + base_rclone_cfg(cfg)
    p = subprocess.run(cmd, text=True, encoding="utf-8", errors="replace",
                       capture_output=True)
    if p.returncode != 0:
        err = (p.stderr or p.stdout).strip()
        if allow_missing and is_not_found(err):
            return {}, False
        raise Fatal(
            "读取云端目录失败。请确认 wpsdav 服务在运行、Cookie 未过期。\n"
            f"命令: {' '.join(cmd)}\n{err}"
        )
    try:
        entries = json.loads(p.stdout or "[]")
    except json.JSONDecodeError as exc:
        raise Fatal(f"无法解析 rclone lsjson 输出: {exc}")
    files = {}
    for ent in entries:
        if ent.get("IsDir"):
            continue
        rel = ent.get("Path")
        if not rel or is_excluded_rel(rel, cfg.excludes, cfg.exclude_dirs, ignored):
            continue
        size = int(ent.get("Size", -1))
        if size < 0:
            raise Fatal(f"云端文件大小未知: {rel}")
        files[rel] = size
    return files, True


def ensure_remote(cfg: Config, ignored: Ignored):
    files, exists = list_remote(cfg, allow_missing=True, ignored=ignored)
    if exists:
        return files
    if cfg.dry_run:
        return {}
    cmd = [cfg.rclone, "mkdir", cfg.remote] + base_rclone_cfg(cfg)
    p = subprocess.run(cmd, text=True, encoding="utf-8", errors="replace",
                       capture_output=True)
    if p.returncode != 0:
        raise Fatal(
            f"创建云端目录失败: {cfg.remote}\n{(p.stderr or p.stdout).strip()}"
        )
    files, exists = list_remote(cfg, allow_missing=False, ignored=ignored)
    if not exists:
        raise Fatal(f"创建后仍无法列出云端目录: {cfg.remote}")
    return files


def same_sig(a, b) -> bool:
    return (a is not None and b is not None
            and a[0] == b[0] and a[1] == b[1])


def decide(sl, sr, lp, rp, lsig, rsize) -> Action:
    no_state = sl is None and sr is None
    if no_state:
        if lp and rp:
            if lsig[0] == rsize:
                return Action("adopt", "两侧同时出现且大小相同")
            return Action("upload", "两侧同时出现但大小不同，保留本地", True)
        if lp:
            return Action("upload", "本地新增")
        if rp:
            return Action("download", "云端新增")
        return Action("nothing")

    if lp and rp:
        if sl is None:
            return Action("upload", "本地新增（云端已有同名文件）",
                          sr != rsize)
        if sr is None:
            lc = not same_sig(sl, lsig)
            if lc:
                return Action("upload", "云端重新出现且本地有修改，保留本地", True)
            if lsig[0] == rsize:
                return Action("adopt", "云端重新出现且大小相同")
            return Action("upload", "云端重新出现但大小不同，保留本地", True)
        lc = not same_sig(sl, lsig)
        rc = sr != rsize
        if not lc and not rc:
            return Action("nothing")
        if lc and not rc:
            return Action("upload", "本地已修改")
        if rc and not lc:
            return Action("download", "云端已修改")
        if lsig[0] == rsize:
            return Action("adopt", "两侧都修改且大小相同，视为已同步", True)
        return Action("upload", "两侧都修改，保留本地", True)

    if lp and not rp:
        if sl is None:
            return Action("upload", "本地新增（云端已删除同名文件）")
        if sr is None:
            if not same_sig(sl, lsig):
                return Action("upload", "云端此前已删除，本地有新修改，重新上传")
            return Action("nothing", "云端此前已删除，保留本地")
        lc = not same_sig(sl, lsig)
        return Action("keep_local", "云端已删除，保留本地" +
                      ("（本地也有修改）" if lc else ""), lc)

    if not lp and rp:
        if sl is None:
            return Action("download", "云端文件，下载到本地")
        if sr is None:
            return Action("download", "本地已删除但云端重新出现，下载保留", True)
        rc = sr != rsize
        if rc:
            return Action("download", "本地已删除但云端有修改，保留云端版本", True)
        return Action("delete_remote", "本地已删除，同步删除云端")

    if sl is None and sr is None:
        return Action("nothing")
    return Action("drop", "两侧都已删除")


def build_actions(local_files, remote_files, state):
    actions = {}
    all_paths = set(local_files) | set(remote_files) | set(state)
    for rel in all_paths:
        ent = state.get(rel)
        sl = ent["l"] if ent else None
        sr = ent["r"] if ent else None
        lsig = local_files.get(rel)
        rsize = remote_files.get(rel)
        actions[rel] = decide(sl, sr, rel in local_files, rel in remote_files,
                              lsig, rsize)
    return actions


def plan_summary(actions, local_files, remote_files):
    out = {
        "upload": [], "download": [], "delete_remote": [],
        "keep_local": [], "adopt": [], "drop": [], "nothing": [],
        "conflicts": [],
    }
    for rel, act in actions.items():
        out[act.kind].append(rel)
        if act.conflict:
            out["conflicts"].append((rel, act.reason))
    upload_bytes = sum(local_files[r][0] for r in out["upload"])
    download_bytes = sum(remote_files[r] for r in out["download"])
    return out, upload_bytes, download_bytes


class Progress:
    def __init__(self, label: str, total_files: int = 0,
                 total_bytes: int = 0, mode: str = "copy"):
        self.label = label
        self.total_files = total_files
        self.total_bytes = total_bytes
        self.mode = mode
        self.tty = sys.stderr.isatty()
        self.last = 0.0
        self.started = False

    def log(self, msg: str):
        if self.tty and self.started:
            sys.stderr.write("\r\x1b[K")
            self.started = False
        eprint(msg)

    def update(self, st: dict):
        now = time.monotonic()
        if not self.tty and now - self.last < 10:
            return
        if self.tty and now - self.last < 0.8:
            return
        self.last = now
        errors = int(st.get("errors", 0) or 0)
        if self.mode == "delete":
            done_files = int(st.get("deletes", 0) or 0)
            total_files = self.total_files or int(st.get("totalTransfers", 0) or 0)
            line = f"[{self.label}] {done_files}/{total_files} 文件"
        else:
            done = int(st.get("bytes", 0) or 0)
            total = self.total_bytes or int(st.get("totalBytes", 0) or 0)
            speed = float(st.get("speed", 0) or 0)
            eta = st.get("eta")
            transfers = int(st.get("transfers", 0) or 0)
            total_transfers = self.total_files or int(
                st.get("totalTransfers", 0) or 0)
            pct = min(100.0, done * 100.0 / total) if total else 0.0
            line = (f"[{self.label}] {pct:5.1f}% {human_bytes(done)}"
                    f"/{human_bytes(total)} {human_bytes(speed)}/s"
                    f" ETA {human_eta(eta)}  {transfers}/{total_transfers} 文件")
        if errors:
            line += f" errors={errors}"
        if self.tty:
            sys.stderr.write("\r" + line + "\x1b[K")
            sys.stderr.flush()
            self.started = True
        else:
            eprint(line)

    def finish(self):
        if self.tty and self.started:
            sys.stderr.write("\n")
            sys.stderr.flush()
            self.started = False


def run_phase(cfg: Config, phase: str, cmd: list, expected: set,
              on_success, flush_state, not_found_ok=False,
              total_files=0, total_bytes=0, size_of=None, mode="copy"):
    """Run one rclone command, parse JSON logs, update state per success."""
    full = cmd + [
        "--use-json-log", "--log-level", "INFO",
        "--stats", cfg.stats_interval, "--stats-one-line",
    ] + base_rclone_cfg(cfg)
    if cfg.verbose:
        eprint(f"[{phase}] 命令: {' '.join(full)}")
    eprint(f"[{phase}] 开始，共 {len(expected)} 个文件"
           + (f" ({human_bytes(total_bytes)})" if total_bytes else ""))
    proc = subprocess.Popen(full, stdout=subprocess.DEVNULL,
                            stderr=subprocess.PIPE, text=True,
                            encoding="utf-8", errors="replace", bufsize=1)
    success = set()
    failures = {}
    global_errors = []
    progress = Progress(phase, total_files, total_bytes, mode)
    last_flush = time.monotonic()
    success_since_flush = 0
    try:
        for raw in proc.stderr:
            raw = raw.strip()
            if not raw:
                continue
            if not raw.startswith("{"):
                eprint(raw)
                continue
            try:
                ev = json.loads(raw)
            except json.JSONDecodeError:
                eprint(raw)
                continue
            st = ev.get("stats")
            if isinstance(st, dict):
                progress.update(st)
                continue
            level = ev.get("level")
            msg = ev.get("msg", "")
            obj = ev.get("object")
            if level == "error":
                err = ev.get("error") or msg
                if (not_found_ok and obj in expected
                        and "not found" in err.lower()):
                    if obj not in success:
                        success.add(obj)
                        try:
                            on_success(obj)
                        except Exception as exc:
                            progress.log(
                                f"[{phase}] 警告: 更新状态失败 {obj}: {exc}")
                        if cfg.verbose:
                            progress.log(f"[{phase}] 已完成（云端已不存在）: {obj}")
                    continue
                if obj in expected:
                    is_new = obj not in failures
                    failures[obj] = err
                    if is_new and len(failures) <= 20:
                        progress.log(f"[{phase}] 失败: {obj}: {err}")
                else:
                    global_errors.append((obj, err))
                continue
            if obj in expected and (
                msg.startswith("Copied") or msg == "Deleted" or msg.startswith("Moved")
            ):
                if obj not in success:
                    success.add(obj)
                    try:
                        on_success(obj)
                    except Exception as exc:  # state update must not abort transfer
                        progress.log(f"[{phase}] 警告: 更新状态失败 {obj}: {exc}")
                    if cfg.verbose:
                        size = size_of(obj) if size_of else None
                        detail = (f" ({human_bytes(size)})"
                                  if size is not None else "")
                        progress.log(f"[{phase}] 完成: {obj}{detail}")
                    success_since_flush += 1
                    now = time.monotonic()
                    if success_since_flush >= 5000 or now - last_flush >= 60:
                        flush_state()
                        last_flush = now
                        success_since_flush = 0
        rc = proc.wait()
    except KeyboardInterrupt:
        eprint(f"\n[{phase}] 收到中断，正在停止 rclone...")
        try:
            proc.send_signal(signal.SIGINT)
            proc.wait(timeout=15)
        except Exception:
            proc.kill()
        flush_state()
        raise
    finally:
        progress.finish()

    for rel in success:
        failures.pop(rel, None)
    unaccounted = expected - success - set(failures)
    for rel in unaccounted:
        failures[rel] = "未确认传输结果"
    for obj, err in global_errors[:20]:
        eprint(f"[{phase}] 全局错误: {obj or '-'}: {err}")
    if len(global_errors) > 20:
        eprint(f"[{phase}] 其余 {len(global_errors) - 20} 条全局错误未显示")
    fail_count = len(failures)
    eprint(f"[{phase}] 完成: 成功 {len(success)}，失败 {fail_count}，退出码 {rc}")
    flush_state()
    return success, failures, rc, global_errors


def write_report(path: Path, lines: list):
    try:
        with open(path, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
    except OSError as exc:
        eprint(f"写报告失败: {exc}")


def parse_args(argv):
    home = Path.home()
    p = argparse.ArgumentParser(
        description="WPS/kDrive 非对称同步：本地删除会删云端，云端删除不动本地",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="示例:\n"
               "  cd ~/Documents/MyDocs && wpsdav-sync.py --remote wps:backup/MyDocs --dry-run\n"
               "  wpsdav-sync.py --local ~/src/proj --remote wps:code/proj "
               "--exclude-dir node_modules --gitignore")
    p.add_argument("--local", default=".",
                   help="本地目录（默认当前目录）")
    p.add_argument("--remote", required=True,
                   help="云端目标目录，例如 wps:backup/MyDocs（必填）")
    p.add_argument("--exclude", action="append", default=[], metavar="PATH",
                   help="不同步的相对路径前缀，可重复；例如 cache 或 private/secret.txt")
    p.add_argument("--exclude-dir", action="append", default=[], metavar="NAME",
                   help="不同步的目录名，任意层级匹配，可重复；例如 node_modules、build")
    p.add_argument("--gitignore", action="store_true",
                   help="跳过 Git 忽略的文件和目录（.gitignore 等）")
    p.add_argument("--state-dir", default=os.environ.get(
        "XDG_STATE_HOME", str(home / ".local" / "state")) + "/wpsdav-sync",
        help="状态目录")
    p.add_argument("--rclone", default=os.environ.get("RCLONE_BIN", "rclone"),
                   help="rclone 可执行文件")
    p.add_argument("--transfers", type=int, default=8, help="并发传输数（默认 8）")
    p.add_argument("--checkers", type=int, default=16, help="并发检查数（默认 16）")
    p.add_argument("--buffer-size", default="32M", help="每个传输的内存缓冲（默认 32M）")
    p.add_argument("--max-delete-pct", type=int, default=10,
                   help="单次最多删除云端文件的比例（默认 10%%）")
    p.add_argument("--stats-interval", default="2s", help="进度刷新间隔")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="逐文件输出成功/失败详情（文件多时建议重定向到日志）")
    p.add_argument("--dry-run", action="store_true", help="只显示计划，不传输不写状态")
    p.add_argument("--force", action="store_true",
                   help="跳过删除数量/空目录等安全阈值")
    return p.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv or sys.argv[1:])
    exclude_dirs = []
    for name in args.exclude_dir:
        name = name.strip().strip("/")
        if not name:
            continue
        if "/" in name or "\\" in name:
            raise Fatal(f"--exclude-dir 只接受目录名，不接受路径: {name}")
        exclude_dirs.append(name)
    cfg = Config(
        local=Path(args.local).expanduser().resolve(),
        remote=args.remote,
        excludes=args.exclude,
        exclude_dirs=exclude_dirs,
        gitignore=args.gitignore,
        state_dir=Path(args.state_dir).expanduser(),
        rclone=args.rclone,
        transfers=args.transfers,
        checkers=args.checkers,
        buffer_size=args.buffer_size,
        dry_run=args.dry_run,
        verbose=args.verbose,
        force=args.force,
        max_delete_pct=args.max_delete_pct,
        stats_interval=args.stats_interval,
    )
    if not shutil_which(cfg.rclone):
        raise Fatal(f"找不到 rclone: {cfg.rclone}")
    if not cfg.local.is_dir():
        raise Fatal(f"本地目录不存在或不是目录: {cfg.local}")
    ignored = collect_gitignored(cfg.local, cfg.gitignore)
    state_path, lock_path, report_path, key = make_state_paths(cfg)
    lock_fd = acquire_lock(lock_path)
    report = []
    try:
        eprint(f"wpsdav-sync: 本地 {cfg.local}")
        eprint(f"wpsdav-sync: 云端 {cfg.remote}"
               + ("  [试运行]" if cfg.dry_run else ""))
        if cfg.gitignore:
            eprint(f"wpsdav-sync: Git 忽略 {len(ignored.dirs)} 个目录、"
                   f"{len(ignored.files)} 个文件")
        state, first_run = load_state(state_path, cfg, ignored)
        if first_run:
            eprint("wpsdav-sync: 首次运行，没有历史状态")

        t0 = time.monotonic()
        eprint("wpsdav-sync: 正在扫描本地...")
        local_files, skipped, skip_prefixes, bad_examples, \
            illegal_examples, empty_examples = scan_local(
                cfg.local, cfg.excludes, set(cfg.exclude_dirs), ignored)
        eprint(f"wpsdav-sync: 本地 {len(local_files)} 个文件，"
               f"{human_bytes(sum(v[0] for v in local_files.values()))}"
               f"；跳过 符号链接 {skipped['symlink']}、"
               f"点项/临时项 {skipped['dot_or_temp']}、"
               f"路径排除 {skipped['excluded_dir']}、"
               f"目录名排除 {skipped['exclude_dir_name']}、"
               f"VCS 忽略 {skipped['vcs_ignored']}、"
               f"其他类型 {skipped['non_regular']}、"
               f"非法文件名 {skipped['bad_name']}、"
               f"WPS 不兼容文件名 {skipped['illegal_name']}、"
               f"空文件 {skipped['empty']}")
        for reason, rel in bad_examples:
            eprint(f"  {reason}: {rel!r}")
        if skipped["bad_name"] > len(bad_examples):
            eprint(f"  其余 {skipped['bad_name'] - len(bad_examples)} 个未显示")
        for reason, rel in illegal_examples:
            eprint(f"  {reason}（改名后才会同步）: {rel}")
        if skipped["illegal_name"] > len(illegal_examples):
            eprint(f"  其余 {skipped['illegal_name'] - len(illegal_examples)} 个未显示")
        for rel in empty_examples:
            eprint(f"  空文件（WPS 无法存储，已跳过）: {rel}")
        if skipped["empty"] > len(empty_examples):
            eprint(f"  其余 {skipped['empty'] - len(empty_examples)} 个未显示")
        if (not local_files and not cfg.force
                and any(e.get("l") is not None for e in state.values())):
            raise Fatal(
                "本地一个文件都没扫描到，但历史状态显示曾有文件。"
                "为防误删云端，已中止；确认本地确实清空后加 --force 重跑"
            )

        eprint("wpsdav-sync: 正在读取云端列表（文件多时可能需要几分钟）...")
        remote_files = ensure_remote(cfg, ignored)
        eprint(f"wpsdav-sync: 云端 {len(remote_files)} 个文件，"
               f"{human_bytes(sum(remote_files.values()))}")

        tracked_remote = sum(1 for e in state.values() if e.get("r") is not None)
        if (tracked_remote > 0 and len(remote_files) == 0 and not cfg.force):
            raise Fatal(
                "云端列表为空但历史状态显示曾有文件。为防误判，已中止；"
                "确认云端确实被清空后加 --force 重跑"
            )

        def under_skip(rel: str) -> bool:
            return any(rel == p or rel.startswith(p + "/") for p in skip_prefixes)

        visible_state = {rel: e for rel, e in state.items() if not under_skip(rel)}
        tracked_remote = sum(1 for e in visible_state.values()
                             if e.get("r") is not None)
        actions = build_actions(local_files, remote_files, visible_state)
        plan, upload_bytes, download_bytes = plan_summary(
            actions, local_files, remote_files)
        conflicts = plan["conflicts"]

        eprint("")
        eprint(f"计划: 上传 {len(plan['upload'])} 个 ({human_bytes(upload_bytes)})"
               f"，下载 {len(plan['download'])} 个 ({human_bytes(download_bytes)})"
               f"，删除云端 {len(plan['delete_remote'])} 个"
               f"，云端删除但保留本地 {len(plan['keep_local'])} 个")
        eprint(f"      已同步 {len(plan['nothing'])} 个，"
               f"按大小视为一致 {len(plan['adopt'])} 个，"
               f"清理状态 {len(plan['drop'])} 个，冲突 {len(conflicts)} 个")
        for rel, reason in conflicts[:20]:
            eprint(f"      冲突: {rel}  ({reason})")
        if len(conflicts) > 20:
            eprint(f"      其余 {len(conflicts) - 20} 个冲突未显示")

        if cfg.dry_run:
            eprint("")
            eprint("[试运行] 未上传、未下载、未删除、未写状态")
            for rel in plan["upload"][:5]:
                eprint(f"  将上传: {rel}")
            for rel in plan["download"][:5]:
                eprint(f"  将下载: {rel}")
            for rel in plan["delete_remote"][:5]:
                eprint(f"  将删除云端: {rel}")
            return 0

        delete_count = len(plan["delete_remote"])
        delete_limit = max(100, tracked_remote * cfg.max_delete_pct // 100)
        if delete_count > delete_limit and not cfg.force:
            raise Fatal(
                f"本次计划删除云端 {delete_count} 个文件，超过安全阈值 "
                f"{delete_limit}。确认无误后加 --force 重跑"
            )

        # Apply decisions that need no transfer.
        for rel in plan["adopt"]:
            lsig = local_files[rel]
            state[rel] = {"l": [lsig[0], lsig[1]], "r": remote_files[rel]}
        for rel in plan["keep_local"]:
            lsig = local_files[rel]
            state[rel] = {"l": [lsig[0], lsig[1]], "r": None}
        for rel in plan["drop"]:
            state.pop(rel, None)
        write_state(state_path, cfg, state)

        # Filter stale paths before invoking rclone.
        uploads = []
        for rel in plan["upload"]:
            full = cfg.local / rel
            try:
                st = os.lstat(full)
            except OSError:
                eprint(f"跳过上传（本地已消失）: {rel}")
                continue
            if not stat.S_ISREG(st.st_mode) or \
                    (st.st_size, st.st_mtime_ns) != local_files[rel]:
                eprint(f"跳过上传（扫描后本地有变化，下次再传）: {rel}")
                continue
            uploads.append(rel)

        downloads = []
        for rel in plan["download"]:
            full = cfg.local / rel
            if full.exists() or full.is_symlink():
                try:
                    st = os.lstat(full)
                    prev = local_files.get(rel)
                    if prev is None or (st.st_size, st.st_mtime_ns) != prev:
                        eprint(f"跳过下载（扫描后本地有变化，避免覆盖）: {rel}")
                        continue
                except OSError:
                    pass
            downloads.append(rel)

        deletes = []
        for rel in plan["delete_remote"]:
            full = cfg.local / rel
            if full.exists() or full.is_symlink():
                eprint(f"跳过云端删除（本地又出现了）: {rel}")
                continue
            deletes.append(rel)

        succeeded = {"upload": set(), "download": set(), "delete": set()}
        all_failures = {}
        global_errors = []

        def flush():
            write_state(state_path, cfg, state)

        if uploads:
            listfile = state_path.parent / f"uploads-{key}.txt"
            listfile.write_text("".join(r + "\n" for r in uploads), encoding="utf-8")
            cmd = [cfg.rclone, "copy", str(cfg.local), cfg.remote,
                   "--files-from-raw", str(listfile),
                   "--no-check-dest", "--no-traverse",
                   "--no-update-dir-modtime",
                   "--transfers", str(cfg.transfers),
                   "--checkers", str(cfg.checkers),
                   "--buffer-size", cfg.buffer_size]

            def on_up(rel):
                lsig = local_files[rel]
                state[rel] = {"l": [lsig[0], lsig[1]], "r": lsig[0]}

            ok, fail, rc, gerr = run_phase(
                cfg, "上传", cmd, set(uploads), on_up, flush,
                total_files=len(uploads),
                total_bytes=sum(local_files[r][0] for r in uploads),
                size_of=lambda rel: local_files[rel][0])
            succeeded["upload"] = ok
            all_failures.update(fail)
            global_errors.extend(gerr)

        if downloads:
            listfile = state_path.parent / f"downloads-{key}.txt"
            listfile.write_text("".join(r + "\n" for r in downloads), encoding="utf-8")
            cmd = [cfg.rclone, "copy", cfg.remote, str(cfg.local),
                   "--files-from-raw", str(listfile),
                   "--no-check-dest", "--no-traverse",
                   "--no-update-dir-modtime",
                   "--transfers", str(cfg.transfers),
                   "--checkers", str(cfg.checkers),
                   "--buffer-size", cfg.buffer_size]

            def on_down(rel):
                rsize = remote_files[rel]
                full = cfg.local / rel
                st = os.lstat(full)
                if stat.S_ISREG(st.st_mode) and st.st_size == rsize:
                    state[rel] = {"l": [st.st_size, st.st_mtime_ns], "r": rsize}
                else:
                    eprint(f"警告: 下载后大小不一致，暂不记录状态: {rel}")

            ok, fail, rc, gerr = run_phase(
                cfg, "下载", cmd, set(downloads), on_down, flush,
                total_files=len(downloads),
                total_bytes=sum(remote_files[r] for r in downloads),
                size_of=lambda rel: remote_files[rel])
            succeeded["download"] = ok
            all_failures.update(fail)
            global_errors.extend(gerr)

        if deletes:
            listfile = state_path.parent / f"deletes-{key}.txt"
            listfile.write_text("".join(r + "\n" for r in deletes), encoding="utf-8")
            cmd = [cfg.rclone, "delete", cfg.remote,
                   "--files-from-raw", str(listfile),
                   "--no-traverse",
                   "--checkers", str(cfg.checkers)]

            def on_del(rel):
                state.pop(rel, None)

            ok, fail, rc, gerr = run_phase(
                cfg, "删除云端", cmd, set(deletes), on_del, flush,
                not_found_ok=True, total_files=len(deletes), mode="delete")
            succeeded["delete"] = ok
            all_failures.update(fail)
            global_errors.extend(gerr)

        write_state(state_path, cfg, state)
        elapsed = time.monotonic() - t0

        eprint("")
        eprint(f"完成，用时 {human_eta(elapsed)}")
        eprint(f"  上传: 成功 {len(succeeded['upload'])}/{len(uploads)}")
        eprint(f"  下载: 成功 {len(succeeded['download'])}/{len(downloads)}")
        eprint(f"  删除云端: 成功 {len(succeeded['delete'])}/{len(deletes)}")
        if all_failures:
            eprint(f"  失败: {len(all_failures)}（详见 {report_path}）")
        if global_errors:
            eprint(f"  全局错误: {len(global_errors)}（详见 {report_path}）")

        report.append(f"wpsdav-sync report {time.strftime('%Y-%m-%d %H:%M:%S')}")
        report.append(f"local={cfg.local}")
        report.append(f"remote={cfg.remote}")
        report.append(f"dry_run={cfg.dry_run}")
        report.append(f"upload={len(succeeded['upload'])}/{len(uploads)}")
        report.append(f"download={len(succeeded['download'])}/{len(downloads)}")
        report.append(f"delete={len(succeeded['delete'])}/{len(deletes)}")
        report.append(f"keep_local={len(plan['keep_local'])}")
        report.append(f"adopt={len(plan['adopt'])}")
        report.append(f"conflicts={len(conflicts)}")
        report.append(f"skipped_bad_names={skipped['bad_name']}")
        report.append(f"skipped_wps_illegal_names={skipped['illegal_name']}")
        report.append(f"skipped_empty_files={skipped['empty']}")
        report.append(f"skipped_exclude_dirs={skipped['exclude_dir_name']}")
        report.append(f"skipped_vcs_ignored={skipped['vcs_ignored']}")
        report.append(f"gitignore={cfg.gitignore}")
        report.append("")
        report.append("## skipped_bad_names")
        report.extend(f"{reason}\t{rel!r}" for reason, rel in bad_examples)
        report.append("")
        report.append("## skipped_wps_illegal_names")
        report.extend(f"{reason}\t{rel}" for reason, rel in illegal_examples)
        report.append("")
        report.append("## skipped_empty_files")
        report.extend(empty_examples)
        report.append("")
        report.append("## conflicts")
        report.extend(f"{rel}\t{reason}" for rel, reason in conflicts)
        report.append("")
        report.append("## failures")
        report.extend(f"{rel}\t{err}" for rel, err in sorted(all_failures.items()))
        report.append("")
        report.append("## global_errors")
        report.extend(f"{obj or '-'}\t{err}" for obj, err in global_errors)
        write_report(report_path, report)

        return 2 if all_failures else 0
    finally:
        try:
            os.close(lock_fd)
        except OSError:
            pass


def shutil_which(name: str):
    from shutil import which
    return which(name)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Fatal as exc:
        eprint(f"错误: {exc}")
        sys.exit(1)
    except KeyboardInterrupt:
        eprint("已中断")
        sys.exit(130)
