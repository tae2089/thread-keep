import os
from pathlib import Path
import subprocess
import sys
from typing import Callable, Mapping, Optional, Sequence

from . import __version__


ProcessRunner = Callable[..., subprocess.CompletedProcess]


def _run(
    command: str,
    arguments: Sequence[str],
    *,
    package_root: Optional[Path] = None,
    environ: Optional[Mapping[str, str]] = None,
    run_process: ProcessRunner = subprocess.run,
    version: str = __version__,
) -> int:
    root = package_root or Path(__file__).resolve().parent
    extension = ".exe" if os.name == "nt" else ""
    executable = root / "bin" / f"{command}{extension}"
    packs = root / "packs"
    if not executable.is_file():
        raise RuntimeError(f"packaged executable is missing: {executable}")
    if not packs.is_dir():
        raise RuntimeError(f"packaged pack directory is missing: {packs}")
    environment = dict(os.environ if environ is None else environ)
    environment["THREAD_KEEP_BUNDLED_PACK_DIR"] = str(packs)
    environment["THREAD_KEEP_BUNDLED_PACK_VERSION"] = version
    result = run_process(
        [str(executable), *arguments],
        check=False,
        env=environment,
    )
    return result.returncode


def thread_keep() -> None:
    raise SystemExit(_run("thread-keep", sys.argv[1:]))


def thread_keep_mcp() -> None:
    raise SystemExit(_run("thread-keep-mcp", sys.argv[1:]))
