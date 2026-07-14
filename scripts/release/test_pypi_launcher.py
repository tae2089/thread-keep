import ast
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest

from pypi.thread_keep import launcher


class LauncherTest(unittest.TestCase):
    def test_launcher_source_is_valid_on_the_declared_python_minimum(self) -> None:
        source = Path(launcher.__file__).read_text(encoding="utf-8")
        ast.parse(source, feature_version=(3, 9))

    def test_run_forwards_arguments_and_pypi_pack_contract(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            package_root = Path(temporary)
            bin_dir = package_root / "bin"
            bin_dir.mkdir()
            executable = bin_dir / ("thread-keep.exe" if os.name == "nt" else "thread-keep")
            executable.write_bytes(b"binary")
            executable.chmod(0o755)
            calls = []

            def run(command, **options):
                calls.append((command, options))
                return subprocess.CompletedProcess(command, 7)

            status = launcher._run(
                "thread-keep",
                ["status", "--json"],
                package_root=package_root,
                environ={"EXISTING": "value"},
                discover_packs=lambda version: {
                    "typescript": {"path": "/packs/typescript", "version": version}
                },
                run_process=run,
                version="1.2.3",
            )

            self.assertEqual(status, 7)
            self.assertEqual(calls[0][0], [str(executable), "status", "--json"])
            self.assertEqual(calls[0][1]["check"], False)
            self.assertEqual(calls[0][1]["env"]["EXISTING"], "value")
            self.assertEqual(
                json.loads(calls[0][1]["env"]["THREAD_KEEP_PYPI_PACKS"]),
                {"typescript": {"path": "/packs/typescript", "version": "1.2.3"}},
            )
            self.assertEqual(calls[0][1]["env"]["THREAD_KEEP_PYTHON_EXECUTABLE"], sys.executable)
            self.assertEqual(calls[0][1]["env"]["THREAD_KEEP_PACKAGE_VERSION"], "1.2.3")

    def test_discover_packs_includes_only_matching_usable_distributions(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            modules = {}
            for language, version, usable in (
                ("typescript", "1.2.3", True),
                ("python", "1.2.2", True),
                ("rust", "1.2.3", False),
            ):
                package = root / f"thread_keep_pack_{language}"
                binary = package / "bin" / f"thread-keep-index-{language}"
                binary.parent.mkdir(parents=True)
                binary.write_bytes(b"binary")
                binary.chmod(0o755 if usable else 0o644)
                modules[f"thread_keep_pack_{language}"] = SimpleNamespace(
                    __file__=str(package / "__init__.py"),
                    __version__=version,
                )

            def import_module(name):
                if name not in modules:
                    raise ModuleNotFoundError(name)
                return modules[name]

            packs = launcher._discover_packs("1.2.3", import_module=import_module)

            expected = root / "thread_keep_pack_typescript" / "bin" / "thread-keep-index-typescript"
            self.assertEqual(
                packs,
                {"typescript": {"path": str(expected.resolve()), "version": "1.2.3"}},
            )


if __name__ == "__main__":
    unittest.main()
