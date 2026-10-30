#!/usr/bin/env python3
"""Validate and exercise a release archive on a darwin-arm64 host (Python 3.9+)."""

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
import uuid

from package import BINARY, PLUGIN_ID, REQUIRED_FILES, archive_name, manifest_version, sha256


def require(condition, message):
    if not condition:
        raise ValueError(message)


def validate_and_extract(path, destination):
    checksum = path.with_name(path.name + ".sha256").read_text(encoding="ascii")
    require(re.fullmatch(r"[0-9a-f]{64}  " + re.escape(path.name) + r"\n", checksum) is not None,
            "checksum must contain one SHA-256 digest and the archive basename only")
    require(sha256(path) == checksum[:64], "archive SHA-256 mismatch")
    with tarfile.open(path, "r:gz") as archive:
        members = {}
        for member in archive.getmembers():
            name = member.name.rstrip("/") if member.isdir() else member.name
            parts = PurePosixPath(name).parts
            require(bool(parts) and parts[0] == PLUGIN_ID and
                    all(part not in (".", "..") for part in parts) and
                    name == PurePosixPath(name).as_posix() and "\\" not in name,
                    "unsafe archive path: " + member.name)
            require(name not in members, "duplicate archive entry: " + name)
            require(member.isdir() or member.isfile(), "archive links/devices are forbidden: " + name)
            expected_mode = 0o755 if member.isdir() or name == PLUGIN_ID + "/" + BINARY else 0o644
            require(member.mode == expected_mode, "incorrect archive permissions: " + name)
            require(member.uid == member.gid == member.mtime == 0 and
                    not member.uname and not member.gname,
                    "archive metadata is not normalized: " + name)
            members[name] = member
        require(PLUGIN_ID in members and members[PLUGIN_ID].isdir(), "missing archive root")
        required = set(REQUIRED_FILES) | {BINARY, "THIRD_PARTY_NOTICES.txt", "licenses/Go/LICENSE"}
        files = {name[len(PLUGIN_ID) + 1:] for name, member in members.items() if member.isfile()}
        require(required <= files, "missing release files: " + ", ".join(sorted(required - files)))
        for relative in files:
            require(relative in required or relative.startswith(("assets/", "licenses/")),
                    "unexpected release content: " + relative)
            require(not any(part.startswith(".") or part == "__pycache__"
                            for part in PurePosixPath(relative).parts),
                    "hidden/cache content in archive: " + relative)
            require(members[PLUGIN_ID + "/" + relative].size > 0, "empty release file: " + relative)
        require(any(name.startswith("assets/") for name in files), "release has no assets")
        for name in members:
            if name == PLUGIN_ID:
                continue
            parent = PurePosixPath(name).parent.as_posix()
            require(parent in members and members[parent].isdir(), "missing archive directory: " + parent)
        for name, member in members.items():
            if member.isdir():
                require(name == PLUGIN_ID or any(file.startswith(name + "/") for file in members
                                                if members[file].isfile()),
                        "unexpected empty directory: " + name)

        def text(relative):
            with archive.extractfile(members[PLUGIN_ID + "/" + relative]) as source:
                return source.read().decode("utf-8")

        version = manifest_version(text("plugin.toml"))
        require(path.name == archive_name(version), "archive name does not match the packaged manifest")
        notices = text("THIRD_PARTY_NOTICES.txt")
        license_paths = re.findall(r"^License: (.+)$", notices, re.MULTILINE)
        require("licenses/Go/LICENSE" in license_paths, "missing Go runtime license notice")
        for dependency in ("github.com/fsnotify/fsnotify", "github.com/rivo/uniseg", "golang.org/x/sys"):
            require(any(name.startswith("licenses/" + dependency + "@") for name in license_paths),
                    "missing compiled dependency license: " + dependency)
        for relative in license_paths + re.findall(r"^Notice: (.+)$", notices, re.MULTILINE):
            require(relative.startswith("licenses/") and relative in files,
                    "notice references missing license content: " + relative)
        with archive.extractfile(members[PLUGIN_ID + "/" + BINARY]) as binary:
            require(binary.read(8) == b"\xcf\xfa\xed\xfe\x0c\x00\x00\x01",
                    "packaged binary is not a 64-bit arm64 Mach-O executable")
        # All paths/types have been checked; copying regular files explicitly
        # avoids tar extraction behavior differences between Python versions.
        for name, member in sorted(members.items()):
            target = destination / name
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(member) as source, target.open("wb") as output:
                    shutil.copyfileobj(source, output)
            target.chmod(member.mode)
    return destination / PLUGIN_ID / BINARY


def run_query(binary, project, env, query, shutdown=False):
    command = [str(binary)] + (["--shutdown"] if shutdown else [])
    result = subprocess.run(command, cwd=project, env=env, input=json.dumps(query),
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=55)
    require(result.returncode == 0, "packaged CLI failed: " + result.stderr.strip())
    if shutdown:
        return None
    result = json.loads(result.stdout)
    require(isinstance(result, list), "packaged CLI must return a destinations array")
    return result


def check_destinations(actual, expected):
    require(len(actual) == len(expected), "unexpected destinations: " + repr(actual))
    seen = set()
    for target in actual:
        key = (target.get("path"), target.get("line"), target.get("col"))
        require(key in expected and key not in seen, "unexpected/duplicate destination: " + repr(target))
        seen.add(key)
        kind, preview, name = expected[key]
        require(target.get("kind") == kind and target.get("preview") == preview,
                "incorrect destination kind/preview: " + repr(target))
        require(target.get("url") == Path(key[0]).as_uri() + "#L{}C{}".format(key[1], key[2]),
                "incorrect destination URL: " + repr(target))
        if name:
            actual_name = target.get("name", "")
            require(actual_name == name or actual_name.endswith("." + name),
                    "incorrect symbol name: " + repr(target))


def smoke(binary, project):
    gopls = os.environ.get("GOPLS") or shutil.which("gopls")
    if not gopls:
        candidate = Path.home() / "go/bin/gopls"
        if candidate.is_file():
            gopls = str(candidate)
    require(bool(gopls), "install gopls or set GOPLS to its executable")
    resolved_gopls = shutil.which(gopls)
    require(bool(resolved_gopls), "gopls is not executable: " + gopls)
    require(shutil.which("go") is not None, "Go must be available on PATH for the release smoke")
    env = dict(os.environ, GOPLS=str(Path(resolved_gopls).resolve()), GOWORK="off", GOPROXY="off",
               GOTOOLCHAIN="local", GOFLAGS="", TERN_WINDOW_KEY="release-smoke-" + uuid.uuid4().hex)
    project.mkdir()
    (project / "go.mod").write_text("module example.com/releasesmoke\n\ngo 1.23\n", encoding="utf-8")
    target, caller, other, closed = [project / name for name in ("target.go", "caller.go", "other.go", "closed.go")]
    saved = {
        target: "package releasesmoke\n\nfunc DiskOriginal() {}\n",
        caller: "package releasesmoke\n\nfunc Caller() { DiskOriginal() }\n",
        other: "package releasesmoke\n\nfunc Other() { DiskOriginal() }\n",
        closed: "package releasesmoke\n\ntype ClosedNeedleSymbol struct{}\n",
    }
    for path, text in saved.items():
        path.write_text(text, encoding="utf-8")
    documents = {
        str(target): "package releasesmoke\n\n\nfunc UnsavedNeedleTarget() {} // unsaved\n",
        str(caller): "package releasesmoke\n\nfunc Caller() { UnsavedNeedleTarget() }\n",
        str(other): "package releasesmoke\n\nfunc Other() { UnsavedNeedleTarget() }\n",
    }
    query = {"path": str(caller), "line": 3, "col": 17, "tabWidth": 4, "documents": documents}
    try:
        navigation = run_query(binary, project, env, query)
        check_destinations(navigation, {
            (str(target), 4, 6): ("definition", "func UnsavedNeedleTarget() {} // unsaved", None),
            (str(caller), 3, 17): ("reference", "func Caller() { UnsavedNeedleTarget() }", None),
            (str(other), 3, 16): ("reference", "func Other() { UnsavedNeedleTarget() }", None),
        })
        require([target["kind"] for target in navigation] == ["definition", "reference", "reference"],
                "combined navigation must put definitions before references")
        symbols = dict(query, action="symbols", search="Needle")
        check_destinations(run_query(binary, project, env, symbols), {
            (str(target), 4, 6): ("symbol", "func UnsavedNeedleTarget() {} // unsaved", "UnsavedNeedleTarget"),
            (str(closed), 3, 6): ("symbol", "type ClosedNeedleSymbol struct{}", "ClosedNeedleSymbol"),
        })
        symbols["search"] = "DiskOriginal"
        check_destinations(run_query(binary, project, env, symbols), {})
        for path, text in saved.items():
            require(path.read_text(encoding="utf-8") == text, "unsaved overlay modified disk: " + str(path))
    finally:
        # The executable, project path and environment are part of daemon
        # identity. Shut down that exact session before deleting either tree.
        run_query(binary, project, env, query, shutdown=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path, help="go-navigation-X.Y.Z-darwin-arm64.tar.gz")
    args = parser.parse_args()
    require(platform.system() == "Darwin" and platform.machine() == "arm64",
            "the packaged release smoke requires a darwin-arm64 host")
    with tempfile.TemporaryDirectory(prefix="go-navigation-smoke-") as temporary:
        root = Path(temporary).resolve()
        binary = validate_and_extract(args.archive.resolve(), root)
        smoke(binary, root / "project")
    print("Release smoke passed: archive, checksum, unsaved combined navigation, and project symbols")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, tarfile.TarError, subprocess.SubprocessError) as error:
        raise SystemExit("Release smoke failed: " + str(error))
