#!/usr/bin/env python3
# -*- coding: utf-8 -*-
# SPDX-License-Identifier: MIT
"""wpsdav-upload: upload selected files/folders to WPS over rclone.

Designed for a Dolphin/KDE service menu: select any files or folders,
right-click, upload to a fixed remote folder.  Paths under the base directory
(default: $HOME) keep their directory structure.  WPS-incompatible remote
names are sanitized (the local files are never renamed).

No content cache is used: a temporary tree of symlinks is created, then
rclone copies through it with --copy-links.  The whole batch is one rclone
process, so multiple files stay fast.
"""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import shutil
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import unquote, urlparse

DEFAULT_ROOT = os.environ.get("WPS_UPLOAD_ROOT", "wps:右键上传")
ILLEGAL = set('\\:*?"<>|')
STATE_DIR = Path(os.environ.get(
    "XDG_STATE_HOME", str(Path.home() / ".local" / "state"))) / "wpsdav-upload"

# Only ~/.ssh needs a confirmation.  Other paths are uploaded if the user
# explicitly selects them.
SENSITIVE = [
    Path.home() / ".ssh",
]


def human_bytes(n) -> str:
    n = float(n or 0)
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
class Config:
    root: str
    base: Path
    mode: str
    include_hidden: bool
    transfers: int
    checkers: int
    buffer_size: str
    dry_run: bool
    no_gui: bool
    assume_yes: bool
    verbose: bool
    stats_interval: str
    confirm_bytes: int
    confirm_files: int


@dataclass
class Item:
    src: Path
    size: int


class UI:
    def __init__(self, enabled: bool):
        self.enabled = enabled and bool(
            os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY"))
        self.kdialog = shutil.which("kdialog")
        self.notify = shutil.which("notify-send")
        self.qdbus = shutil.which("qdbus6") or shutil.which("qdbus")
        self.proc = None
        self.service = None
        self.path = None
        self.progress_ok = False
        self.last_update = 0.0

    def _kdialog(self, args):
        try:
            return subprocess.run([self.kdialog] + args, timeout=60,
                                  capture_output=True, text=True)
        except Exception:
            return None

    def info(self, title: str, msg: str):
        if self.enabled and self.notify:
            subprocess.run([self.notify, "-a", "WPS 上传", "-i", "cloud-upload",
                            title, msg], timeout=10, capture_output=True)
        elif self.enabled and self.kdialog:
            self._kdialog(["--title", title, "--passivepopup", msg, "8"])
        else:
            print(f"{title}: {msg}", file=sys.stderr, flush=True)

    def error(self, title: str, msg: str):
        if self.enabled and self.kdialog:
            self._kdialog(["--title", title, "--error", msg])
        elif self.enabled and self.notify:
            subprocess.run([self.notify, "-a", "WPS 上传", "-u", "critical",
                            title, msg], timeout=10, capture_output=True)
        else:
            print(f"{title}: {msg}", file=sys.stderr, flush=True)

    def confirm(self, title: str, msg: str) -> bool:
        if self.enabled and self.kdialog:
            r = self._kdialog(["--title", title, "--warningyesno", msg])
            return r is not None and r.returncode == 0
        return False

    def progress_start(self, title: str) -> bool:
        if not (self.enabled and self.kdialog and self.qdbus):
            return False
        try:
            self.proc = subprocess.Popen(
                [self.kdialog, "--progressbar", title, "100"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
            ref = self.proc.stdout.readline().strip()
            parts = ref.split()
            if len(parts) < 2:
                self.proc.terminate()
                return False
            self.service, self.path = parts[0], parts[1]
            self.progress_ok = True
            return True
        except Exception:
            return False

    def progress_update(self, pct: int, label: str):
        if not self.progress_ok:
            return
        now = time.monotonic()
        if now - self.last_update < 0.5:
            return
        self.last_update = now
        try:
            subprocess.run([self.qdbus, self.service, self.path,
                            "org.kde.kdialog.ProgressDialog.value", str(pct)],
                           timeout=5, capture_output=True)
            subprocess.run([self.qdbus, self.service, self.path,
                            "org.kde.kdialog.ProgressDialog.setLabelText", label],
                           timeout=5, capture_output=True)
        except Exception:
            self.progress_ok = False

    def progress_close(self):
        if self.proc is None:
            return
        try:
            if self.progress_ok:
                subprocess.run([self.qdbus, self.service, self.path,
                                "org.kde.kdialog.ProgressDialog.close"],
                               timeout=5, capture_output=True)
            self.proc.wait(timeout=3)
        except Exception:
            try:
                self.proc.terminate()
            except Exception:
                pass
        self.proc = None
        self.progress_ok = False


def parse_args(argv):
    home = Path.home()
    p = argparse.ArgumentParser(
        description="把选中的文件/文件夹上传到 WPS 云盘，保持家目录结构")
    p.add_argument("paths", nargs="*", metavar="PATH", help="文件或文件夹")
    p.add_argument("--root", default=DEFAULT_ROOT,
                   help=f"云端根目录（默认 {DEFAULT_ROOT}）")
    p.add_argument("--base", default=str(home),
                   help="计算相对路径的本地根（默认 $HOME）")
    p.add_argument("--mode", choices=("overwrite", "timestamp"),
                   default="overwrite",
                   help="overwrite=覆盖同名文件；timestamp=按时间建子目录")
    p.add_argument("--exclude-hidden", dest="include_hidden",
                   action="store_false", default=True,
                   help="递归文件夹时跳过点文件和点目录（默认包含）")
    p.add_argument("--transfers", type=int, default=8, help="并发传输数")
    p.add_argument("--checkers", type=int, default=16, help="并发检查数")
    p.add_argument("--buffer-size", default="32M", help="每个传输的内存缓冲")
    p.add_argument("--stats-interval", default="1s", help="进度刷新间隔")
    p.add_argument("--dry-run", action="store_true", help="只显示映射，不传输")
    p.add_argument("--no-gui", action="store_true", help="不弹窗、不显示进度条")
    p.add_argument("-y", "--yes", dest="assume_yes", action="store_true",
                   help="跳过敏感文件/大体积选择确认")
    p.add_argument("-v", "--verbose", action="store_true", help="逐文件输出")
    args = p.parse_args(argv)
    return args


def is_under(p: Path, root: Path) -> bool:
    try:
        p.relative_to(root)
        return True
    except ValueError:
        return False


def sensitive_hit(src: Path) -> bool:
    src = Path(os.path.realpath(src))
    for raw in SENSITIVE:
        s = Path(os.path.realpath(raw))
        if src == s or is_under(src, s) or is_under(s, src):
            return True
    return False


def sanitize_component(part: str) -> str:
    out = []
    for ch in part:
        if ch in ILLEGAL or ord(ch) < 32 or ord(ch) == 127:
            out.append("_")
        else:
            out.append(ch)
    s = "".join(out).rstrip(" .")
    if not s:
        s = "_"
    if s.startswith("."):
        s = "_" + s.lstrip(".")
    return s


def remote_rel(src: Path, base: Path) -> str:
    try:
        rel = os.path.relpath(str(src), str(base))
        if rel == "." or rel == ".." or rel.startswith(".." + os.sep):
            raise ValueError
    except ValueError:
        rel = "external/" + str(src).lstrip("/")
    parts = [sanitize_component(p)
             for p in rel.replace(os.sep, "/").split("/") if p not in ("", ".")]
    if not parts:
        raise ValueError("empty relative path")
    return "/".join(parts)


def collect(paths, cfg: Config, skipped: dict):
    items = {}
    inputs = []
    for arg in paths:
        if not arg:
            continue
        if arg.startswith("file://"):
            arg = unquote(urlparse(arg).path)
        for piece in arg.splitlines():
            piece = piece.strip()
            if piece:
                inputs.append(piece)

    base_real = Path(os.path.realpath(cfg.base))
    for raw in inputs:
        p = Path(raw).expanduser()
        if not p.exists() and not p.is_symlink():
            skipped["missing"] += 1
            continue
        p = p.absolute()
        real = Path(os.path.realpath(p))
        if real == base_real or real in base_real.parents:
            raise Fatal("不能直接上传整个家目录或其父目录，请选择具体文件/子目录")

        if p.is_dir():
            def onerror(exc):
                skipped["unreadable"] += 1
            for dp, dns, fns in os.walk(p, followlinks=False, onerror=onerror):
                hidden_dirs = [d for d in dns if d.startswith(".")]
                link_dirs = [d for d in dns
                             if os.path.islink(os.path.join(dp, d))]
                if not cfg.include_hidden:
                    skipped["hidden"] += len(hidden_dirs)
                    dns[:] = [d for d in dns if not d.startswith(".")]
                skipped["symlink_dir"] += len(link_dirs)
                dns[:] = [d for d in dns
                          if not os.path.islink(os.path.join(dp, d))]
                for name in fns:
                    if not cfg.include_hidden and name.startswith("."):
                        skipped["hidden"] += 1
                        continue
                    fp = Path(dp) / name
                    if fp.is_symlink() and fp.is_dir():
                        skipped["symlink_dir"] += 1
                        continue
                    if fp.is_symlink() and not fp.exists():
                        skipped["broken"] += 1
                        continue
                    if not fp.is_file():
                        skipped["non_regular"] += 1
                        continue
                    try:
                        size = fp.stat().st_size
                    except OSError:
                        skipped["unreadable"] += 1
                        continue
                    if size == 0:
                        skipped["empty"] += 1
                        continue
                    items[str(fp)] = Item(fp, size)
        elif p.is_file():
            try:
                size = p.stat().st_size
            except OSError:
                skipped["unreadable"] += 1
                continue
            if size == 0:
                skipped["empty"] += 1
                continue
            items[str(p)] = Item(p, size)
        else:
            skipped["non_regular"] += 1
    return [items[k] for k in sorted(items)]


def build_mappings(items, cfg: Config):
    used = {}
    mappings = []
    renamed = 0
    for item in items:
        try:
            rel = remote_rel(item.src, cfg.base)
        except ValueError:
            rel = "external/" + sanitize_component(item.src.name)
        candidate = rel
        n = 2
        while candidate in used:
            stem, ext = os.path.splitext(rel)
            candidate = f"{stem}~{n}{ext}"
            n += 1
        used[candidate] = item
        original = os.path.relpath(str(item.src), str(cfg.base)).replace(os.sep, "/") \
            if is_under(item.src, cfg.base) else "external/" + str(item.src).lstrip("/")
        if candidate != original:
            renamed += 1
        mappings.append((item, candidate))
    return mappings, renamed


def make_staging(mappings, staging: Path):
    ok = []
    conflicts = []
    for item, rel in mappings:
        dst = staging / rel
        try:
            dst.parent.mkdir(parents=True, exist_ok=True)
        except (FileExistsError, NotADirectoryError):
            conflicts.append(rel)
            continue
        try:
            os.symlink(str(item.src), dst)
            ok.append((item, rel))
        except FileExistsError:
            conflicts.append(rel)
    return ok, conflicts


def remote_root_for(cfg: Config) -> str:
    root = cfg.root.rstrip("/")
    if cfg.mode == "timestamp":
        root += "/" + time.strftime("%Y-%m-%d_%H-%M-%S")
    return root


def preflight(cfg: Config):
    """Return (ok, message).  Checks auth/reachability before scanning."""
    cmd = ["rclone", "lsf", cfg.root, "--dirs-only", "--max-depth", "1",
           "--retries", "1", "--low-level-retries", "1",
           "--contimeout", "10s", "--timeout", "30s"]
    p = subprocess.run(cmd, text=True, encoding="utf-8", errors="replace",
                       capture_output=True)
    if p.returncode == 0:
        return True, ""
    err = (p.stderr or p.stdout).strip()
    low = err.lower()
    if "usernotlogin" in low or "用户未登录" in err:
        return False, (
            "WPS 登录已过期（userNotLogin）。\n"
            "请先更新 Cookie：\n"
            "  wpsdav cookie -c ~/.config/wpsdav/config.json\n"
            "  wpsdav doctor -c ~/.config/wpsdav/config.json\n"
            "然后重试。"
        )
    if "not found" in low or "not exist" in low:
        return True, ""
    if "connection refused" in low or "connect: connection refused" in low:
        return False, ("无法连接 wpsdav。\n"
                       "请先启动：systemctl --user start wpsdav")
    return False, f"无法访问云端目录 {cfg.root}:\n{err}"


def run_upload(cfg: Config, ui: UI, mappings, root: str, log_lines,
               skipped: dict, renamed: int, dry_run: bool):
    staging = STATE_DIR / f"staging-{os.getpid()}"
    listfile = STATE_DIR / f"list-{os.getpid()}.txt"
    for old in STATE_DIR.glob("staging-*"):
        shutil.rmtree(old, ignore_errors=True)
    shutil.rmtree(staging, ignore_errors=True)
    staging.mkdir(parents=True, exist_ok=True)
    os.chmod(staging, 0o700)
    try:
        staged, conflicts = make_staging(mappings, staging)
        if conflicts:
            log_lines.append("## staging conflicts")
            log_lines.extend(conflicts)
        if not staged:
            raise Fatal("没有可上传的文件")

        rels = [rel for _, rel in staged]
        listfile.write_text("".join(r + "\n" for r in rels), encoding="utf-8")
        cmd = [
            "rclone", "copy", str(staging), root,
            "--files-from-raw", str(listfile),
            "--copy-links", "--no-check-dest", "--no-traverse",
            "--no-update-dir-modtime",
            "--transfers", str(cfg.transfers),
            "--checkers", str(cfg.checkers),
            "--buffer-size", cfg.buffer_size,
            "--use-json-log", "--log-level", "INFO",
            "--stats", cfg.stats_interval, "--stats-one-line",
            "--retries", "3", "--retries-sleep", "5s",
            "--low-level-retries", "10", "--contimeout", "30s",
            "--timeout", "30m",
        ]
        log_lines.append("## command")
        log_lines.append(" ".join(cmd))
        if dry_run:
            print(f"目标: {root}")
            print(f"文件: {len(staged)} 个，"
                  f"{human_bytes(sum(i.size for i, _ in staged))}")
            print(f"跳过: {skipped}")
            if renamed:
                print(f"云端文件名被规范化: {renamed} 个")
            print("映射（前 30 个）:")
            for item, rel in staged[:30]:
                print(f"  {item.src} -> {root}/{rel}")
            if len(staged) > 30:
                print(f"  ... 其余 {len(staged) - 30} 个")
            print("命令:", " ".join(cmd))
            return 0, set(), {}, [], 0, ""

        expected = {rel: item.size for item, rel in staged}
        total_bytes = sum(expected.values())
        if total_bytes >= 1024 * 1024:
            ui.progress_start("上传到 WPS 云盘")
        start = time.monotonic()
        success = set()
        failures = {}
        globals_ = []
        copied_bytes = 0
        try:
            proc = subprocess.Popen(cmd, stdout=subprocess.DEVNULL,
                                    stderr=subprocess.PIPE, text=True,
                                    encoding="utf-8", errors="replace", bufsize=1)
            last_print = 0.0
            for raw in proc.stderr:
                raw = raw.strip()
                if not raw:
                    continue
                if not raw.startswith("{"):
                    globals_.append(raw)
                    continue
                try:
                    ev = json.loads(raw)
                except json.JSONDecodeError:
                    continue
                st = ev.get("stats")
                if isinstance(st, dict):
                    done = int(st.get("bytes", 0) or 0)
                    speed = float(st.get("speed", 0) or 0)
                    pct = min(100, int(done * 100 / total_bytes)) if total_bytes else 0
                    label = (f"{pct}%  {human_bytes(done)}/{human_bytes(total_bytes)}"
                             f"  {human_bytes(speed)}/s  ETA {human_eta(st.get('eta'))}")
                    ui.progress_update(pct, label)
                    now = time.monotonic()
                    if cfg.verbose or (not ui.progress_ok and now - last_print >= 2):
                        print(f"[上传] {label}", file=sys.stderr, flush=True)
                        last_print = now
                    continue
                level = ev.get("level")
                msg = ev.get("msg", "")
                obj = ev.get("object")
                if level == "error":
                    err = ev.get("error") or msg
                    if obj in expected:
                        failures[obj] = err
                        print(f"[失败] {obj}: {err}", file=sys.stderr, flush=True)
                    else:
                        globals_.append(f"{obj or '-'}: {err}")
                    continue
                if obj in expected and msg.startswith("Copied"):
                    if obj not in success:
                        success.add(obj)
                        copied_bytes += expected[obj]
                        if cfg.verbose:
                            print(f"[完成] {obj} ({human_bytes(expected[obj])})",
                                  file=sys.stderr, flush=True)
            rc = proc.wait()
        finally:
            ui.progress_close()

        for rel in success:
            failures.pop(rel, None)
        for rel in set(expected) - success - set(failures):
            failures[rel] = "未确认传输结果"

        elapsed = time.monotonic() - start
        avg = copied_bytes / elapsed if elapsed > 0 else 0
        skipped_msg = "、".join(f"{k} {v}" for k, v in skipped.items() if v)
        summary = (f"{len(success)}/{len(expected)} 个文件，"
                   f"{human_bytes(copied_bytes)}，平均 {human_bytes(avg)}/s，"
                   f"用时 {human_eta(elapsed)}")
        if skipped_msg:
            summary += f"\n跳过: {skipped_msg}"
        if renamed:
            summary += f"\n云端文件名规范化: {renamed} 个（详见日志）"
        summary += f"\n云端: {root}"

        log_lines.append("## summary")
        log_lines.append(summary)
        log_lines.append(f"rc={rc} success={len(success)} failures={len(failures)}")
        if failures:
            log_lines.append("## failures")
            log_lines.extend(f"{k}\t{v}" for k, v in sorted(failures.items()))
        if globals_:
            log_lines.append("## global errors")
            log_lines.extend(globals_)
        return (0 if rc == 0 and not failures else 2,
                success, failures, globals_, copied_bytes, summary)
    finally:
        shutil.rmtree(staging, ignore_errors=True)
        try:
            listfile.unlink()
        except OSError:
            pass


def main(argv=None) -> int:
    args = parse_args(argv or sys.argv[1:])
    cfg = Config(
        root=args.root,
        base=Path(args.base).expanduser().absolute(),
        mode=args.mode,
        include_hidden=args.include_hidden,
        transfers=args.transfers,
        checkers=args.checkers,
        buffer_size=args.buffer_size,
        dry_run=args.dry_run,
        no_gui=args.no_gui,
        assume_yes=args.assume_yes,
        verbose=args.verbose,
        stats_interval=args.stats_interval,
        confirm_bytes=int(float(os.environ.get("WPS_UPLOAD_CONFIRM_GB", "5")) * 1024**3),
        confirm_files=int(os.environ.get("WPS_UPLOAD_CONFIRM_FILES", "5000")),
    )
    ui = UI(not cfg.no_gui)
    if not args.paths:
        ui.info("WPS 上传", "没有选择文件")
        return 1
    if not shutil.which("rclone"):
        ui.error("WPS 上传失败", "找不到 rclone")
        return 1

    STATE_DIR.mkdir(parents=True, exist_ok=True)
    lock_path = STATE_DIR / "lock"
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        ui.error("WPS 上传", "已有上传任务在进行，请等它完成")
        os.close(lock_fd)
        return 1

    log_path = STATE_DIR / "last.log"
    log_lines = [f"wpsdav-upload {time.strftime('%Y-%m-%d %H:%M:%S')}",
                 f"base={cfg.base}", f"root={cfg.root}", f"mode={cfg.mode}",
                 f"include_hidden={cfg.include_hidden}"]
    skipped = {"hidden": 0, "empty": 0, "symlink_dir": 0, "broken": 0,
               "non_regular": 0, "missing": 0, "unreadable": 0}
    try:
        ok, preflight_msg = preflight(cfg)
        if not ok:
            log_lines.append(f"PREFLIGHT: {preflight_msg}")
            ui.error("WPS 上传失败", preflight_msg)
            return 1

        items = collect(args.paths, cfg, skipped)
        if not items:
            msg = "没有可上传的文件"
            detail = "、".join(f"{k} {v}" for k, v in skipped.items() if v)
            if detail:
                msg += f"（跳过: {detail}）"
            ui.info("WPS 上传", msg)
            return 0

        mappings, renamed = build_mappings(items, cfg)
        total_bytes = sum(i.size for i in items)
        if total_bytes > cfg.confirm_bytes or len(items) > cfg.confirm_files:
            if not cfg.assume_yes:
                ok = ui.confirm(
                    "确认上传",
                    f"将上传 {len(items)} 个文件，共 {human_bytes(total_bytes)}。\n"
                    f"目标: {remote_root_for(cfg)}\n继续？")
                if not ok:
                    ui.info("WPS 上传", "已取消")
                    return 0
        if any(sensitive_hit(i.src) for i in items) and not cfg.assume_yes:
            ok = ui.confirm(
                "敏感文件确认",
                "选择中包含 ~/.ssh（SSH 私钥/配置）。\n"
                "上传到云端会泄露这些内容。确定继续？")
            if not ok:
                ui.info("WPS 上传", "已取消（敏感文件）")
                return 0

        root = remote_root_for(cfg)
        log_lines.append(f"target={root}")
        log_lines.append(f"items={len(items)} bytes={total_bytes} renamed={renamed}")
        log_lines.append("## mappings")
        for item, rel in mappings:
            log_lines.append(f"{item.src}\t->\t{root}/{rel}")
        rc, success, failures, globals_, copied, summary = run_upload(
            cfg, ui, mappings, root, log_lines, skipped, renamed, cfg.dry_run)
        if cfg.dry_run:
            return 0
        if failures or rc != 0:
            detail = " ".join(globals_) + " " + " ".join(failures.values())
            if "usernotlogin" in detail.lower() or "用户未登录" in detail:
                ui.error("WPS 登录已过期",
                         "请先更新 wpsdav 的 Cookie：\n"
                         "  wpsdav cookie -c ~/.config/wpsdav/config.json\n"
                         "  wpsdav doctor -c ~/.config/wpsdav/config.json\n"
                         "然后重新右键上传。")
            else:
                ui.error("WPS 上传失败",
                         f"{summary}\n失败: {len(failures)}\n日志: {log_path}")
            return 2
        ui.info("WPS 上传完成", summary)
        return 0
    except Fatal as exc:
        log_lines.append(f"FATAL: {exc}")
        ui.error("WPS 上传失败", str(exc))
        return 1
    except Exception as exc:
        log_lines.append(f"ERROR: {exc}")
        ui.error("WPS 上传失败", f"{exc}\n日志: {log_path}")
        return 1
    finally:
        try:
            log_path.write_text("\n".join(log_lines) + "\n", encoding="utf-8")
        except OSError:
            pass
        try:
            os.close(lock_fd)
        except OSError:
            pass


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("已中断", file=sys.stderr)
        sys.exit(130)
