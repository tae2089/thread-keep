#!/usr/bin/env python3
import argparse
import base64
import csv
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import stat
import tempfile
from typing import Mapping, Sequence
import zipfile


STABLE_SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
REPOSITORY = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
WHEEL_TAG = re.compile(r"^[A-Za-z0-9_.]+$")
ZIP_TIMESTAMP = (1980, 1, 1, 0, 0, 0)


def _load_config(path: Path) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise ValueError(f"read release configuration: {error}") from error
    if not isinstance(value, dict):
        raise ValueError("release configuration must be a JSON object")
    for key in ("wheel_binaries", "packs", "targets"):
        if not isinstance(value.get(key), list) or not value[key]:
            raise ValueError(f"release configuration {key} must be a non-empty array")
    if not all(isinstance(binary, str) and binary for binary in value["wheel_binaries"]):
        raise ValueError("release configuration contains an invalid wheel binary")
    for pack in value["packs"]:
        if not isinstance(pack, dict) or not isinstance(pack.get("id"), str) or not pack["id"]:
            raise ValueError("release configuration contains an invalid pack")
    required_target_fields = ("id", "goos", "goarch", "wheelTag")
    for target in value["targets"]:
        if not isinstance(target, dict) or any(not isinstance(target.get(field), str) or not target[field] for field in required_target_fields):
            raise ValueError("release configuration contains an invalid target")
        if not WHEEL_TAG.fullmatch(target["wheelTag"]):
            raise ValueError(f"release target {target['id']} has an invalid wheel tag")
    for values, label in (
        (value["wheel_binaries"], "wheel binary"),
        ([pack["id"] for pack in value["packs"]], "pack ID"),
        ([target["id"] for target in value["targets"]], "target ID"),
        ([target["wheelTag"] for target in value["targets"]], "wheel tag"),
    ):
        if len(values) != len(set(values)):
            raise ValueError(f"release configuration contains a duplicate {label}")
    return value


def _artifact_name(binary: str, target: Mapping[str, str]) -> str:
    extension = ".exe" if target["goos"] == "windows" else ""
    return f"{binary}_{target['goos']}_{target['goarch']}{extension}"


def _validate_inputs(
    artifacts_dir: Path,
    config: Mapping,
    license_file: Path,
    repository: str,
    template_dir: Path,
    version: str,
) -> None:
    if not STABLE_SEMVER.fullmatch(version):
        raise ValueError("wheel version must be stable SemVer X.Y.Z")
    if not REPOSITORY.fullmatch(repository):
        raise ValueError("wheel repository must be owner/name")
    if not license_file.is_file():
        raise ValueError("wheel license file is missing")
    if not (template_dir / "launcher.py").is_file():
        raise ValueError("wheel launcher template is missing")
    binaries = [*config["wheel_binaries"], *(pack["id"] for pack in config["packs"])]
    for target in config["targets"]:
        for binary in binaries:
            artifact = artifacts_dir / _artifact_name(binary, target)
            if not artifact.is_file():
                raise ValueError(f"missing wheel artifact: {artifact.name}")


def _core_metadata(packs: Sequence[Mapping[str, str]], repository: str, version: str) -> bytes:
    lines = [
        "Metadata-Version: 2.3\n"
        "Name: thread-keep\n"
        f"Version: {version}\n"
        "Summary: Versioned local code context for humans and coding agents\n"
        "License: MIT\n"
        "Requires-Python: >=3.9\n"
        f"Project-URL: Repository, https://github.com/{repository}\n"
    ]
    for pack in packs:
        language = pack["language"]
        distribution = f"thread-keep-pack-{language}"
        lines.append(f"Provides-Extra: {language}\n")
        lines.append(f'Requires-Dist: {distribution}=={version}; extra == "{language}"\n')
    lines.append("Provides-Extra: all\n")
    for pack in packs:
        distribution = f"thread-keep-pack-{pack['language']}"
        lines.append(f'Requires-Dist: {distribution}=={version}; extra == "all"\n')
    lines.append("\n")
    return "".join(lines).encode()


def _pack_metadata(language: str, repository: str, version: str) -> bytes:
    return (
        "Metadata-Version: 2.3\n"
        f"Name: thread-keep-pack-{language}\n"
        f"Version: {version}\n"
        f"Summary: Native {language} indexer pack for Thread Keep\n"
        "License: MIT\n"
        "Requires-Python: >=3.9\n"
        f"Project-URL: Repository, https://github.com/{repository}\n"
        "\n"
    ).encode()


def _wheel_metadata(tag: str) -> bytes:
    return (
        "Wheel-Version: 1.0\n"
        "Generator: thread-keep release tooling\n"
        "Root-Is-Purelib: false\n"
        f"Tag: py3-none-{tag}\n"
    ).encode()


def _record(files: Mapping[str, bytes], record_path: str) -> bytes:
    output = io.StringIO(newline="")
    writer = csv.writer(output, lineterminator="\n")
    for name in sorted(files):
        contents = files[name]
        digest = base64.urlsafe_b64encode(hashlib.sha256(contents).digest()).rstrip(b"=").decode()
        writer.writerow((name, f"sha256={digest}", len(contents)))
    writer.writerow((record_path, "", ""))
    return output.getvalue().encode()


def _write_member(archive: zipfile.ZipFile, name: str, contents: bytes, mode: int) -> None:
    info = zipfile.ZipInfo(name, ZIP_TIMESTAMP)
    info.create_system = 3
    info.compress_type = zipfile.ZIP_DEFLATED
    info.external_attr = (stat.S_IFREG | mode) << 16
    archive.writestr(info, contents, compresslevel=9)


def _write_wheel(path: Path, files: dict[str, bytes], executable_paths: set[str]) -> Path:
    with zipfile.ZipFile(path, "w") as archive:
        for name in sorted(files):
            _write_member(archive, name, files[name], 0o755 if name in executable_paths else 0o644)
    return path


def _build_core_wheel(
    artifacts_dir: Path,
    config: Mapping,
    license_file: Path,
    output_dir: Path,
    repository: str,
    target: Mapping[str, str],
    template_dir: Path,
    version: str,
) -> Path:
    dist_info = f"thread_keep-{version}.dist-info"
    files: dict[str, bytes] = {
        "thread_keep/__init__.py": f'__version__ = "{version}"\n'.encode(),
        "thread_keep/launcher.py": (template_dir / "launcher.py").read_bytes(),
        f"{dist_info}/LICENSE": license_file.read_bytes(),
        f"{dist_info}/METADATA": _core_metadata(config["packs"], repository, version),
        f"{dist_info}/WHEEL": _wheel_metadata(target["wheelTag"]),
        f"{dist_info}/entry_points.txt": (
            "[console_scripts]\n"
            "thread-keep = thread_keep.launcher:thread_keep\n"
            "thread-keep-mcp = thread_keep.launcher:thread_keep_mcp\n"
        ).encode(),
    }
    executable_paths: set[str] = set()
    extension = ".exe" if target["goos"] == "windows" else ""
    for binary in config["wheel_binaries"]:
        name = f"thread_keep/bin/{binary}{extension}"
        files[name] = (artifacts_dir / _artifact_name(binary, target)).read_bytes()
        executable_paths.add(name)
    record_path = f"{dist_info}/RECORD"
    files[record_path] = _record(files, record_path)
    distribution_dir = output_dir / "thread-keep"
    distribution_dir.mkdir(exist_ok=True)
    return _write_wheel(
        distribution_dir / f"thread_keep-{version}-py3-none-{target['wheelTag']}.whl",
        files,
        executable_paths,
    )


def _build_pack_wheel(
    artifacts_dir: Path,
    license_file: Path,
    output_dir: Path,
    pack: Mapping[str, str],
    repository: str,
    target: Mapping[str, str],
    version: str,
) -> Path:
    language = pack["language"]
    distribution = f"thread-keep-pack-{language}"
    normalized = distribution.replace("-", "_")
    dist_info = f"{normalized}-{version}.dist-info"
    extension = ".exe" if target["goos"] == "windows" else ""
    executable = f"{normalized}/bin/{pack['id']}{extension}"
    files = {
        f"{normalized}/__init__.py": f'__version__ = "{version}"\n'.encode(),
        executable: (artifacts_dir / _artifact_name(pack["id"], target)).read_bytes(),
        f"{dist_info}/LICENSE": license_file.read_bytes(),
        f"{dist_info}/METADATA": _pack_metadata(language, repository, version),
        f"{dist_info}/WHEEL": _wheel_metadata(target["wheelTag"]),
    }
    record_path = f"{dist_info}/RECORD"
    files[record_path] = _record(files, record_path)
    distribution_dir = output_dir / distribution
    distribution_dir.mkdir(exist_ok=True)
    return _write_wheel(
        distribution_dir / f"{normalized}-{version}-py3-none-{target['wheelTag']}.whl",
        files,
        {executable},
    )


def _replace_directory(staged: Path, output: Path) -> None:
    backup = output.with_name(f".{output.name}.backup")
    if backup.exists():
        shutil.rmtree(backup)
    had_output = output.exists()
    if had_output:
        os.replace(output, backup)
    try:
        os.replace(staged, output)
    except OSError:
        if had_output:
            os.replace(backup, output)
        raise
    if backup.exists():
        shutil.rmtree(backup)


def build_wheels(
    *,
    artifacts_dir: Path,
    config_file: Path,
    license_file: Path,
    output_dir: Path,
    repository: str,
    template_dir: Path,
    version: str,
) -> list[Path]:
    config = _load_config(config_file)
    _validate_inputs(artifacts_dir, config, license_file, repository, template_dir, version)
    output_dir.parent.mkdir(parents=True, exist_ok=True)
    staged = Path(tempfile.mkdtemp(prefix=f".{output_dir.name}-", dir=output_dir.parent))
    try:
        for target in config["targets"]:
            _build_core_wheel(artifacts_dir, config, license_file, staged, repository, target, template_dir, version)
            for pack in config["packs"]:
                _build_pack_wheel(artifacts_dir, license_file, staged, pack, repository, target, version)
        _replace_directory(staged, output_dir)
    except BaseException:
        shutil.rmtree(staged, ignore_errors=True)
        raise
    return sorted(output_dir.glob("*/*.whl"))


def _arguments(arguments: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Build Thread Keep platform wheels from staged GoReleaser artifacts")
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--license", dest="license_file", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--template", type=Path, required=True)
    parser.add_argument("--version", required=True)
    return parser.parse_args(arguments)


def main(arguments: Sequence[str] | None = None) -> int:
    values = _arguments(arguments)
    wheels = build_wheels(
        artifacts_dir=values.artifacts,
        config_file=values.config,
        license_file=values.license_file,
        output_dir=values.out,
        repository=values.repository,
        template_dir=values.template,
        version=values.version,
    )
    for wheel in wheels:
        print(wheel)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
