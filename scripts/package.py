#!/usr/bin/env python3
"""Build the darwin-arm64 release using Python 3.9+ and the Go toolchain."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parent.parent
PLUGIN_ID = "go-navigation"
TARGET = "darwin-arm64"
VERSION_PATTERN = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
BINARY = "bin/tern-go-navigation"
REQUIRED_FILES = (
    "plugin.toml",
    "window.luau",
    "README.md",
    "README.zh-CN.md",
    "docs/reference/changelog.md",
)
LICENSE_NAMES = ("LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING")


def manifest_version(text):
    # The repository manifest is deliberately flat. Reject tables rather than
    # accidentally accepting an id/version from a nested TOML namespace.
    if re.search(r"^\s*\[", text, re.MULTILINE):
        raise ValueError("plugin.toml must have flat top-level id and version")
    values = {}
    for key in ("id", "version"):
        assignments = re.findall(r"^\s*" + key + r"\s*=([^\n]*)$", text, re.MULTILINE)
        if len(assignments) != 1:
            raise ValueError("plugin.toml must contain exactly one " + key)
        match = re.fullmatch(r'\s*"([^"\\\r\n]*)"\s*(?:#.*)?', assignments[0])
        if not match:
            raise ValueError("plugin.toml " + key + " must be a plain quoted scalar")
        values[key] = match.group(1)
    if values["id"] != PLUGIN_ID:
        raise ValueError("plugin.toml id must be " + PLUGIN_ID)
    if not re.fullmatch(VERSION_PATTERN, values["version"]):
        raise ValueError("plugin.toml version must be a stable X.Y.Z version")
    return values["version"]


def archive_name(version):
    return "{}-{}-{}.tar.gz".format(PLUGIN_ID, version, TARGET)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def go_output(args, env):
    return subprocess.check_output(["go"] + args, cwd=ROOT, env=env, text=True).strip()


def json_objects(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]


def copy_file(source, destination):
    if source.is_symlink() or not source.is_file():
        raise ValueError("release input must be a regular file: " + str(source))
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)


def add_licenses(stage, env):
    goroot = Path(go_output(["env", "GOROOT"], env))
    toolchain = go_output(["version"], env)
    license_file = goroot / "LICENSE"
    # Homebrew keeps the Go license beside libexec, not inside GOROOT.
    if not license_file.is_file() and goroot.name == "libexec":
        license_file = goroot.parent / "LICENSE"
    copy_file(license_file.resolve(strict=True), stage / "licenses/Go/LICENSE")
    notices = [
        "Third-party software included in bin/tern-go-navigation",
        "",
        toolchain,
        "License: licenses/Go/LICENSE",
    ]
    # List packages for the exact release target, not every module in go.sum.
    module_paths = sorted(set(go_output([
        "list", "-mod=readonly", "-deps", "-f",
        "{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}{{end}}{{end}}", ".",
    ], env).split()))
    if module_paths:
        modules = json_objects(go_output(["list", "-mod=readonly", "-m", "-json"] + module_paths, env))
        for module in modules:
            if module.get("Replace"):
                raise ValueError("release dependencies must not use replace: " + module["Path"])
            name, version = module["Path"], module["Version"]
            if not re.fullmatch(r"[A-Za-z0-9._/+~-]+", name) or any(
                part in ("", ".", "..") for part in name.split("/")
            ) or not re.fullmatch(r"v[0-9][A-Za-z0-9.+-]*", version):
                raise ValueError("unsafe dependency identity: " + name + "@" + version)
            directory = Path(module["Dir"])
            licenses = [directory / name for name in LICENSE_NAMES if (directory / name).is_file()]
            if not licenses:
                raise ValueError("no recognized license for compiled dependency " + name)
            notices.extend(["", name + " " + version])
            for license_file in licenses:
                relative = Path("licenses") / (name + "@" + version) / license_file.name
                copy_file(license_file, stage / relative)
                notices.append("License: " + relative.as_posix())
            for notice_name in ("NOTICE", "NOTICE.txt"):
                notice_file = directory / notice_name
                if notice_file.is_file():
                    relative = Path("licenses") / (name + "@" + version) / notice_name
                    copy_file(notice_file, stage / relative)
                    notices.append("Notice: " + relative.as_posix())
    (stage / "THIRD_PARTY_NOTICES.txt").write_text("\n".join(notices) + "\n", encoding="utf-8")


def write_archive(stage, destination):
    # Normalize gzip and tar metadata; source mtimes, ownership and umask must
    # not leak into the archive. Go's executable content remains toolchain-bound.
    with destination.open("wb") as output:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for path in [stage] + sorted(stage.rglob("*")):
                    relative = path.relative_to(stage.parent).as_posix()
                    info = tarfile.TarInfo(relative)
                    info.uid = info.gid = info.mtime = 0
                    info.uname = info.gname = ""
                    if path.is_dir():
                        info.type = tarfile.DIRTYPE
                        info.mode = 0o755
                        archive.addfile(info)
                    else:
                        info.mode = 0o755 if path == stage / BINARY else 0o644
                        info.size = path.stat().st_size
                        with path.open("rb") as source:
                            archive.addfile(info, source)


def package(output, version):
    env = dict(os.environ, CGO_ENABLED="0", GOOS="darwin", GOARCH="arm64",
               GOWORK="off", GOFLAGS="", GOTOOLCHAIN="local")
    output.mkdir(parents=True, exist_ok=True)
    # Keep temporary outputs on the destination filesystem for atomic replace.
    with tempfile.TemporaryDirectory(prefix=".go-navigation-package-", dir=output) as temporary:
        temporary = Path(temporary)
        stage = temporary / PLUGIN_ID
        stage.mkdir()
        for relative in REQUIRED_FILES:
            copy_file(ROOT / relative, stage / relative)
        assets = ROOT / "assets"
        if assets.is_symlink() or not assets.is_dir():
            raise ValueError("assets must be a real directory")
        for source in sorted(assets.rglob("*")):
            relative = source.relative_to(ROOT)
            if any(part.startswith(".") or part == "__pycache__" for part in relative.parts):
                continue
            if source.is_symlink():
                raise ValueError("release assets must not be symlinks: " + str(source))
            if not source.is_dir():
                copy_file(source, stage / relative)
        if not (stage / "assets").is_dir():
            raise ValueError("release must contain assets")
        binary = stage / BINARY
        binary.parent.mkdir()
        subprocess.run([
            "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(binary), ".",
        ], cwd=ROOT, env=env, check=True)
        binary.chmod(0o755)
        add_licenses(stage, env)
        name = archive_name(version)
        archive = temporary / name
        write_archive(stage, archive)
        checksum = temporary / (name + ".sha256")
        checksum.write_text(sha256(archive) + "  " + name + "\n", encoding="ascii")
        archive.replace(output / name)
        checksum.replace(output / checksum.name)
    print(output / name)
    print(output / (name + ".sha256"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=Path("dist"), help="output directory (default: dist)")
    tags = parser.add_mutually_exclusive_group()
    tags.add_argument("--tag", help="require the tag to equal v<plugin.toml version> before packaging")
    tags.add_argument("--check-tag", help="validate this tag without building or writing output")
    args = parser.parse_args()
    version = manifest_version((ROOT / "plugin.toml").read_text(encoding="utf-8"))
    tag = args.tag if args.tag is not None else args.check_tag
    if tag is not None and tag != "v" + version:
        parser.error("tag must equal v" + version + " from plugin.toml")
    if args.check_tag is not None:
        print("Validated " + tag)
        return
    package(args.output.resolve(), version)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        raise SystemExit("Packaging failed: " + str(error))
