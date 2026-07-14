import ast
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from pypi.thread_keep import launcher


class LauncherTest(unittest.TestCase):
    def test_launcher_source_is_valid_on_the_declared_python_minimum(self) -> None:
        source = Path(launcher.__file__).read_text(encoding="utf-8")
        ast.parse(source, feature_version=(3, 9))

    def test_run_forwards_arguments_and_bundled_pack_contract(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            package_root = Path(temporary)
            bin_dir = package_root / "bin"
            packs_dir = package_root / "packs"
            bin_dir.mkdir()
            packs_dir.mkdir()
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
                run_process=run,
                version="1.2.3",
            )

            self.assertEqual(status, 7)
            self.assertEqual(calls[0][0], [str(executable), "status", "--json"])
            self.assertEqual(calls[0][1]["check"], False)
            self.assertEqual(calls[0][1]["env"]["EXISTING"], "value")
            self.assertEqual(calls[0][1]["env"]["THREAD_KEEP_BUNDLED_PACK_DIR"], str(packs_dir))
            self.assertEqual(calls[0][1]["env"]["THREAD_KEEP_BUNDLED_PACK_VERSION"], "1.2.3")

    def test_run_rejects_missing_pack_directory_before_starting_process(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            package_root = Path(temporary)
            bin_dir = package_root / "bin"
            bin_dir.mkdir()
            executable = bin_dir / ("thread-keep.exe" if os.name == "nt" else "thread-keep")
            executable.write_bytes(b"binary")

            with self.assertRaisesRegex(RuntimeError, "pack directory is missing"):
                launcher._run("thread-keep", [], package_root=package_root, version="1.2.3")


if __name__ == "__main__":
    unittest.main()
