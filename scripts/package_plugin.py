#!/usr/bin/env python3
"""Build and sign the standalone STATE Kit plugin. Never packages local runtime data."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = ROOT / "plugin"
VERSION = "4.6.1"
PLUGIN_ID = "io.github.wangyunjeff.sub2api-state-kit"
KEY_ID = "state-kit-local-v1"
PLATFORMS = ("linux-amd64", "linux-arm64", "darwin-arm64")


def run(args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def write_zip(path: Path, files: dict[str, bytes]):
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for name, data in sorted(files.items()):
            info = zipfile.ZipInfo(name, date_time=(2026, 9, 24, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = (0o100755 if name.startswith("runtimes/") else 0o100644) << 16
            archive.writestr(info, data)


def public_der(openssl: str, key: Path) -> bytes:
    der = subprocess.check_output([openssl, "pkey", "-in", str(key), "-pubout", "-outform", "DER"])
    if len(der) != 44 or der[:12] != bytes.fromhex("302a300506032b6570032100"):
        raise ValueError("Signing key must be Ed25519")
    return der


def verify(package: Path, public: Path, openssl: str):
    with zipfile.ZipFile(package) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)):
            raise ValueError("Duplicate package entries")
        manifest_bytes = archive.read("manifest.json")
        manifest = json.loads(manifest_bytes)
        signature = json.loads(archive.read("signature.json"))
        if set(names) != set(manifest["files"]) | {"manifest.json", "signature.json"}:
            raise ValueError("Manifest and package entries differ")
        for name, expected in manifest["files"].items():
            if name.startswith("/") or ".." in name.split("/") or "\\" in name:
                raise ValueError("Unsafe package path")
            if hashlib.sha256(archive.read(name)).hexdigest() != expected:
                raise ValueError("Payload hash mismatch: " + name)
    raw_key = base64.b64decode(public.read_text().strip(), validate=True)
    if len(raw_key) != 32 or signature["algorithm"] != "ed25519" or signature["key_id"] != KEY_ID:
        raise ValueError("Unexpected publisher signature")
    with tempfile.TemporaryDirectory(prefix="state-kit-verify-") as temp:
        d = Path(temp)
        (d / "public.der").write_bytes(bytes.fromhex("302a300506032b6570032100") + raw_key)
        (d / "manifest.json").write_bytes(manifest_bytes)
        (d / "signature.bin").write_bytes(base64.b64decode(signature["signature"], validate=True))
        run([openssl, "pkeyutl", "-verify", "-pubin", "-inkey", str(d / "public.der"), "-keyform", "DER", "-rawin", "-in", str(d / "manifest.json"), "-sigfile", str(d / "signature.bin")])
    print(json.dumps({"verified": True, "plugin_id": manifest["id"], "version": manifest["version"], "platforms": list(manifest["runtimes"]), "payload_files": len(manifest["files"])}, ensure_ascii=False))


def source_archive(destination: Path):
    # Explicit source-file allowlist; no .git, builds, databases, env files or credentials.
    files = {}
    allowed = {".go", ".mod", ".sum", ".proto", ".json", ".md", ".html", ".js", ".cjs", ".css", ".py", ".txt", ".yaml"}
    for path in sorted(PLUGIN.rglob("*")):
        rel = path.relative_to(ROOT)
        if path.is_symlink() or not path.is_file() or path.suffix not in allowed:
            continue
        if set(rel.parts) & {"build", "dist", "node_modules", "__pycache__", ".git"}:
            continue
        if path.name.startswith(".") or path.name.endswith(".log"):
            continue
        files[rel.as_posix()] = path.read_bytes()
    for relative in ("LICENSE", "NOTICE", "scripts/package_plugin.py", "docs/plugin.md", "docs/plugin-validation.md"):
        path = ROOT / relative
        if path.exists():
            files[relative] = path.read_bytes()
    files["README.md"] = ("# Sub2API STATE Kit 插件源码\n\n"
                          "这是独立插件源码，不是完整 Sub2API 宿主。\n\n"
                          "- [安装与使用](docs/plugin.md)\n"
                          "- [插件开发与测试](plugin/README.md)\n"
                          "- [验证范围](docs/plugin-validation.md)\n\n"
                          "插件仓库：https://github.com/zhang2580384/sub2api-state-kit\n"
                          "原完整宿主版与增量版：https://github.com/wangyunjeff/sub2api-state-kit\n").encode()
    write_zip(destination, files)


def build(args):
    key = args.private_key.expanduser().resolve()
    if key.is_relative_to(ROOT):
        raise ValueError("Keep the release signing private key OUTSIDE the source repository")
    # NTFS cannot represent Unix owner-only bits through pathlib's st_mode.
    if os.name != "nt" and key.stat().st_mode & 0o077:
        raise ValueError("Signing private key must be readable only by its owner (chmod 600)")
    expected = base64.b64decode((PLUGIN / "release/publisher-public-key.txt").read_text().strip(), validate=True)
    if public_der(args.openssl, key)[12:] != expected:
        raise ValueError("Signing key does not match the committed publisher public key")
    output = args.output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    prebuilt = {}
    for item in args.prebuilt_runtime:
        platform, separator, raw_path = item.partition("=")
        if not separator or not platform or not raw_path:
            raise ValueError("--prebuilt-runtime must be PLATFORM=PATH")
        path = Path(raw_path).expanduser().resolve()
        if not path.is_file():
            raise ValueError("prebuilt runtime does not exist: " + platform)
        prebuilt[platform] = path
    files, runtimes = {}, {}
    with tempfile.TemporaryDirectory(prefix="state-kit-package-") as temp:
        d = Path(temp)
        for platform in args.platforms:
            goos, arch = platform.split("-", 1)
            if platform not in PLATFORMS:
                raise ValueError("Unsupported build platform: " + platform)
            binary = prebuilt.get(platform) or d / ("state-kit-" + platform)
            if platform not in prebuilt:
                env = {**os.environ, "CGO_ENABLED": "0", "GOOS": goos, "GOARCH": arch}
                run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(binary), "./cmd/state-kit"], cwd=PLUGIN, env=env)
            name = f"runtimes/{platform}/state-kit"
            files[name] = binary.read_bytes()
            runtimes[platform] = {"path": name}
        for path in sorted((PLUGIN / "ui").rglob("*")):
            if path.is_file():
                if path.is_symlink() or path.suffix not in {".html", ".js", ".css"}:
                    raise ValueError("Unexpected UI payload: " + str(path))
                files[path.relative_to(PLUGIN).as_posix()] = path.read_bytes()
        manifest = {
            "schema_version": 1, "id": PLUGIN_ID, "name": "STATE Kit · 账号级票据",
            "version": VERSION, "description": "按账号启用 Pro / Team STATE，支持稳定同出口与 Cookie 分流，覆盖生成器或 S5/HTTP 打票、可选质量探针、备用票、上一轮 IP 复用、地区过滤、请求时区与语言统一、实时诊断和异常重采。",
            "author": "zhang2580384 / Sub2API STATE Kit",
            "requires": {"sub2api": ">=0.2.8 <0.3.0", "recommended_sub2api_version": "0.2.8", "tested_sub2api_versions": ["0.2.8"], "plugin_protocol": 1, "transport_api": 1, "ui_bridge": 1},
            "capabilities": [{"id": "openai.oauth.outbound_transport.v1", "platform": "openai", "account_type": "oauth"}],
            "runtimes": runtimes, "ui": {"entrypoint": "ui/index.html"},
            "files": {name: hashlib.sha256(data).hexdigest() for name, data in sorted(files.items())},
        }
        files["manifest.json"] = (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode()
        (d / "manifest.json").write_bytes(files["manifest.json"])
        run([args.openssl, "pkeyutl", "-sign", "-inkey", str(key), "-rawin", "-in", str(d / "manifest.json"), "-out", str(d / "signature.bin")])
        files["signature.json"] = (json.dumps({"algorithm": "ed25519", "key_id": KEY_ID, "signature": base64.b64encode((d / "signature.bin").read_bytes()).decode()}, indent=2) + "\n").encode()
        package = output / f"sub2api-state-kit_plugin_v{VERSION}.s2plugin"
        write_zip(package, files)
    shutil.copyfile(PLUGIN / "release/publisher-public-key.txt", output / "publisher-public-key.txt")
    shutil.copyfile(PLUGIN / "release/trusted-publisher.yaml", output / "trusted-publisher.yaml")
    source_archive(output / f"sub2api-state-kit_plugin_v{VERSION}_source.zip")
    verify(package, output / "publisher-public-key.txt", args.openssl)
    names = [package.name, f"sub2api-state-kit_plugin_v{VERSION}_source.zip", "publisher-public-key.txt", "trusted-publisher.yaml"]
    (output / "SHA256SUMS").write_text("".join(f"{hashlib.sha256((output / name).read_bytes()).hexdigest()}  {name}\n" for name in sorted(names)))
    print("Artifacts: " + str(output))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--openssl", default=shutil.which("openssl") or "openssl")
    sub = parser.add_subparsers(dest="command", required=True)
    b = sub.add_parser("build")
    b.add_argument("--private-key", type=Path, required=True)
    b.add_argument("--output", type=Path, required=True)
    b.add_argument("--platforms", nargs="+", default=list(PLATFORMS))
    b.add_argument("--prebuilt-runtime", action="append", default=[], metavar="PLATFORM=PATH")
    v = sub.add_parser("verify")
    v.add_argument("--package", type=Path, required=True)
    v.add_argument("--public-key", type=Path, default=PLUGIN / "release/publisher-public-key.txt")
    args = parser.parse_args()
    if args.command == "build":
        build(args)
    else:
        verify(args.package, args.public_key, args.openssl)


if __name__ == "__main__":
    main()
