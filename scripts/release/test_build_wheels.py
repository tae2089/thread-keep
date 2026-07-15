import base64
import csv
from email.parser import BytesParser
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import zipfile

from scripts.release.build_wheels import build_wheels


ROOT = Path(__file__).resolve().parents[2]
CONFIG_FILE = ROOT / "scripts" / "release" / "release-config.json"


class BuildWheelsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.artifacts = self.root / "artifacts"
        self.artifacts.mkdir()
        self.license_file = self.root / "LICENSE"
        self.license_file.write_text("test license\n", encoding="utf-8")
        self.readme_file = self.root / "README.md"
        self.readme_file.write_text("# Thread Keep\n\nVersioned project description.\n", encoding="utf-8")
        self.template_dir = self.root / "template"
        self.template_dir.mkdir()
        (self.template_dir / "launcher.py").write_text(
            "def thread_keep(): pass\ndef thread_keep_mcp(): pass\n",
            encoding="utf-8",
        )

    def seed_artifacts(self) -> dict:
        config = json.loads(CONFIG_FILE.read_text(encoding="utf-8"))
        binaries = [*config["wheel_binaries"], *(pack["id"] for pack in config["packs"])]
        for target in config["targets"]:
            extension = ".exe" if target["goos"] == "windows" else ""
            for binary in binaries:
                artifact = self.artifacts / f"{binary}_{target['goos']}_{target['goarch']}{extension}"
                artifact.write_bytes(f"{binary}:{target['id']}".encode())
                artifact.chmod(0o755)
        return config

    def build(self, output: Path, version: str = "1.2.3") -> list[Path]:
        return build_wheels(
            artifacts_dir=self.artifacts,
            config_file=CONFIG_FILE,
            license_file=self.license_file,
            output_dir=output,
            readme_file=self.readme_file,
            repository="tae2089/thread-keep",
            template_dir=self.template_dir,
            version=version,
        )

    def test_rejects_invalid_version_without_writing_output(self) -> None:
        self.seed_artifacts()
        output = self.root / "wheels"

        with self.assertRaisesRegex(ValueError, "stable SemVer"):
            self.build(output, "1.2.3-beta")

        self.assertFalse(output.exists())

    def test_rejects_incomplete_artifacts_without_writing_output(self) -> None:
        config = self.seed_artifacts()
        target = config["targets"][0]
        missing = self.artifacts / f"thread-keep-index-rust_{target['goos']}_{target['goarch']}"
        missing.unlink()
        output = self.root / "wheels"

        with self.assertRaisesRegex(ValueError, "missing wheel artifact"):
            self.build(output)

        self.assertFalse(output.exists())

    def test_rejects_missing_license_without_writing_output(self) -> None:
        self.seed_artifacts()
        self.license_file.unlink()
        output = self.root / "wheels"

        with self.assertRaisesRegex(ValueError, "wheel license file is missing"):
            self.build(output)

        self.assertFalse(output.exists())

    def test_rejects_missing_readme_without_writing_output(self) -> None:
        self.seed_artifacts()
        self.readme_file.unlink()
        output = self.root / "wheels"

        with self.assertRaisesRegex(ValueError, "wheel README file is missing"):
            self.build(output)

        self.assertFalse(output.exists())

    def test_builds_deterministic_platform_wheels_with_complete_records(self) -> None:
        config = self.seed_artifacts()
        first = self.build(self.root / "first")
        second = self.build(self.root / "second")

        self.assertEqual(len(first), len(config["targets"]) * (len(config["packs"]) + 1))
        self.assertEqual(
            [path.relative_to(self.root / "first") for path in first],
            [path.relative_to(self.root / "second") for path in second],
        )
        for left, right in zip(first, second, strict=True):
            self.assertEqual(hashlib.sha256(left.read_bytes()).digest(), hashlib.sha256(right.read_bytes()).digest())

        wheel = next(
            path
            for path in first
            if path.parent.name == "thread-keep" and "manylinux_2_39_x86_64" in path.name
        )
        with zipfile.ZipFile(wheel) as archive:
            names = set(archive.namelist())
            dist_info = "thread_keep-1.2.3.dist-info"
            expected_binaries = {
                "thread_keep/bin/thread-keep",
                "thread_keep/bin/thread-keep-mcp",
            }
            self.assertTrue(expected_binaries <= names)
            self.assertFalse(any(name.startswith("thread_keep/packs/") for name in names))
            self.assertIn("thread_keep/launcher.py", names)
            self.assertIn(f"{dist_info}/LICENSE", names)

            metadata = archive.read(f"{dist_info}/METADATA").decode()
            self.assertIn("Name: thread-keep\n", metadata)
            self.assertIn("Version: 1.2.3\n", metadata)
            self.assertIn("Requires-Python: >=3.9\n", metadata)
            for pack in config["packs"]:
                language = pack["language"]
                distribution = f"thread-keep-pack-{language}"
                self.assertIn(f"Provides-Extra: {language}\n", metadata)
                self.assertIn(
                    f'Requires-Dist: {distribution}==1.2.3; extra == "{language}"\n',
                    metadata,
                )
                self.assertIn(
                    f'Requires-Dist: {distribution}==1.2.3; extra == "all"\n',
                    metadata,
                )
            self.assertIn("Provides-Extra: all\n", metadata)

            wheel_metadata = archive.read(f"{dist_info}/WHEEL").decode()
            self.assertIn("Root-Is-Purelib: false\n", wheel_metadata)
            self.assertIn("Tag: py3-none-manylinux_2_39_x86_64\n", wheel_metadata)

            entry_points = archive.read(f"{dist_info}/entry_points.txt").decode()
            self.assertIn("thread-keep = thread_keep.launcher:thread_keep\n", entry_points)
            self.assertIn("thread-keep-mcp = thread_keep.launcher:thread_keep_mcp\n", entry_points)

            for name in expected_binaries:
                mode = archive.getinfo(name).external_attr >> 16
                self.assertEqual(mode & 0o777, 0o755)

            records = list(csv.reader(io.StringIO(archive.read(f"{dist_info}/RECORD").decode())))
            self.assertEqual({row[0] for row in records}, names)
            for name, digest, size in records:
                if name == f"{dist_info}/RECORD":
                    self.assertEqual((digest, size), ("", ""))
                    continue
                contents = archive.read(name)
                encoded = base64.urlsafe_b64encode(hashlib.sha256(contents).digest()).rstrip(b"=").decode()
                self.assertEqual(digest, f"sha256={encoded}")
                self.assertEqual(size, str(len(contents)))

        pack = config["packs"][0]
        pack_wheel = next(
            path
            for path in first
            if path.parent.name == f"thread-keep-pack-{pack['language']}"
            and "manylinux_2_39_x86_64" in path.name
        )
        with zipfile.ZipFile(pack_wheel) as archive:
            names = set(archive.namelist())
            module = f"thread_keep_pack_{pack['language']}"
            dist_info = f"thread_keep_pack_{pack['language']}-1.2.3.dist-info"
            executable = f"{module}/bin/{pack['id']}"
            self.assertEqual(
                names,
                {
                    f"{module}/__init__.py",
                    executable,
                    f"{dist_info}/LICENSE",
                    f"{dist_info}/METADATA",
                    f"{dist_info}/WHEEL",
                    f"{dist_info}/RECORD",
                },
            )
            metadata = archive.read(f"{dist_info}/METADATA").decode()
            self.assertIn(f"Name: thread-keep-pack-{pack['language']}\n", metadata)
            self.assertIn("Version: 1.2.3\n", metadata)
            self.assertEqual(archive.getinfo(executable).external_attr >> 16 & 0o777, 0o755)

    def test_core_wheel_metadata_includes_markdown_project_description(self) -> None:
        self.seed_artifacts()
        wheels = build_wheels(
            artifacts_dir=self.artifacts,
            config_file=CONFIG_FILE,
            license_file=self.license_file,
            output_dir=self.root / "wheels",
            readme_file=self.readme_file,
            repository="tae2089/thread-keep",
            template_dir=self.template_dir,
            version="1.2.3",
        )
        wheel = next(
            path
            for path in wheels
            if path.parent.name == "thread-keep" and "manylinux_2_39_x86_64" in path.name
        )

        with zipfile.ZipFile(wheel) as archive:
            metadata = BytesParser().parsebytes(archive.read("thread_keep-1.2.3.dist-info/METADATA"))

        self.assertEqual(metadata["Description-Content-Type"], "text/markdown")
        self.assertEqual(metadata.get_payload(), "# Thread Keep\n\nVersioned project description.\n")

    def test_pip_install_preserves_packaged_executable_modes(self) -> None:
        self.seed_artifacts()
        wheels = self.build(self.root / "wheels")
        wheel = next(
            path
            for path in wheels
            if path.parent.name == "thread-keep-pack-typescript"
            and "manylinux_2_39_x86_64" in path.name
        )
        installed = self.root / "installed"

        result = subprocess.run(
            [
                sys.executable,
                "-m",
                "pip",
                "install",
                "--no-deps",
                "--only-binary=:all:",
                "--platform=manylinux_2_39_x86_64",
                "--implementation=py",
                "--python-version=3.9",
                "--abi=none",
                f"--target={installed}",
                str(wheel),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        packaged = installed / "thread_keep_pack_typescript" / "bin" / "thread-keep-index-typescript"
        self.assertTrue(packaged.is_file())
        self.assertNotEqual(packaged.stat().st_mode & 0o111, 0)


if __name__ == "__main__":
    unittest.main()
