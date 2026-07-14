import importlib
import json
import os
from pathlib import Path
import subprocess
import sys
from typing import Callable, Mapping, Optional, Sequence

from . import __version__


ProcessRunner = Callable[..., subprocess.CompletedProcess]
PackDiscovery = Callable[[str], Mapping[str, Mapping[str, str]]]

PACKS = (
    ("typescript", "thread_keep_pack_typescript", "thread-keep-index-typescript"),
    ("javascript", "thread_keep_pack_javascript", "thread-keep-index-javascript"),
    ("python", "thread_keep_pack_python", "thread-keep-index-python"),
    ("java", "thread_keep_pack_java", "thread-keep-index-java"),
    ("kotlin", "thread_keep_pack_kotlin", "thread-keep-index-kotlin"),
    ("rust", "thread_keep_pack_rust", "thread-keep-index-rust"),
)


def _discover_packs(version: str, *, import_module: Callable[[str], object] = importlib.import_module) -> dict:
    extension = ".exe" if os.name == "nt" else ""
    packs = {}
    for language, module_name, binary_name in PACKS:
        try:
            module = import_module(module_name)
        except ModuleNotFoundError:
            continue
        module_file = getattr(module, "__file__", None)
        if getattr(module, "__version__", None) != version or not module_file:
            continue
        executable = Path(module_file).resolve().parent / "bin" / f"{binary_name}{extension}"
        if not executable.is_file() or os.name != "nt" and not os.access(executable, os.X_OK):
            continue
        packs[language] = {"path": str(executable), "version": version}
    return packs


def _run(
    command: str,
    arguments: Sequence[str],
    *,
    package_root: Optional[Path] = None,
    environ: Optional[Mapping[str, str]] = None,
    discover_packs: PackDiscovery = _discover_packs,
    run_process: ProcessRunner = subprocess.run,
    version: str = __version__,
) -> int:
    root = package_root or Path(__file__).resolve().parent
    extension = ".exe" if os.name == "nt" else ""
    executable = root / "bin" / f"{command}{extension}"
    if not executable.is_file():
        raise RuntimeError(f"packaged executable is missing: {executable}")
    environment = dict(os.environ if environ is None else environ)
    environment["THREAD_KEEP_PYPI_PACKS"] = json.dumps(discover_packs(version), separators=(",", ":"), sort_keys=True)
    environment["THREAD_KEEP_PYTHON_EXECUTABLE"] = sys.executable
    environment["THREAD_KEEP_PACKAGE_VERSION"] = version
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
