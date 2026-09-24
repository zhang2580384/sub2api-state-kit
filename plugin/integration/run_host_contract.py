#!/usr/bin/env python3
"""Test our signed package using an existing stock Sub2API v0.2.8 checkout.

No git mutations, host downloads, or host service deployment are performed. Go may download
missing module dependencies through its normal module cache. The package must
contain a runtime for this machine. All upstream requests use localhost fixtures.

Example:
  python3 plugin/integration/run_host_contract.py \
    --host-src /path/to/sub2api-v0.2.8 \
    --package /path/to/state-kit.s2plugin \
    --public-key /path/to/publisher-public-key.txt \
    --log /path/to/host-contract.log
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host-src", required=True, type=Path)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--public-key", required=True, help="32-byte public key in base64, or text-file path")
    parser.add_argument("--go", default="go", help="Go executable (host requires Go 1.27)")
    parser.add_argument("--log", type=Path, help="Write the same test output to this file")
    args = parser.parse_args()

    host = args.host_src.expanduser().resolve()
    package = args.package.expanduser().resolve()
    harness = Path(__file__).with_name("host_contract_test.go.txt")
    service = host / "backend" / "internal" / "service"
    target = service / "state_kit_external_plugin_test.go"
    if not (service / "plugin_host_services.go").is_file():
        parser.error("--host-src must point to stock Sub2API v0.2.8 with HostService support")
    if not package.is_file():
        parser.error("--package does not exist")
    if target.exists():
        parser.error(f"refusing to overwrite pre-existing test: {target}")
    public_key = args.public_key.strip()
    key_file = Path(public_key).expanduser()
    if key_file.is_file():
        public_key = key_file.read_text(encoding="utf-8").strip()
    try:
        decoded = base64.b64decode(public_key, validate=True)
        if len(decoded) != 32:
            raise ValueError("key must contain exactly 32 bytes")
    except (ValueError, TypeError) as error:
        parser.error(f"invalid Ed25519 public key: {error}")

    env = os.environ.copy()
    env["STATE_KIT_TEST_PACKAGE"] = str(package)
    env["STATE_KIT_TEST_PUBLIC_KEY"] = public_key
    command = [args.go, "test", "./internal/service", "-tags", "statekit_host_contract",
               "-run", "^TestStateKitOfficialHost", "-count=1", "-timeout=4m", "-v"]
    log = None
    if args.log:
        args.log.parent.mkdir(parents=True, exist_ok=True)
        log = args.log.open("w", encoding="utf-8")
    try:
        shutil.copyfile(harness, target)
        metadata = [
            "Running real stock-host installer and plugin process tests; localhost upstreams only.",
            f"Package SHA256: {hashlib.sha256(package.read_bytes()).hexdigest()}",
            f"Platform: {platform.system()} {platform.machine()}",
            "Host compatibility: Sub2API 0.2.8; publisher key ID: state-kit-local-v1",
        ]
        if (host / ".git").exists():
            revision = subprocess.run(["git", "rev-parse", "HEAD"], cwd=host,
                                      text=True, capture_output=True, check=True)
            metadata.append(f"Host checkout commit: {revision.stdout.strip()}")
        for line in metadata:
            print(line, flush=True)
            if log:
                log.write(line + "\n")
                log.flush()
        process = subprocess.Popen(command, cwd=host / "backend", env=env,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                   text=True, bufsize=1)
        assert process.stdout is not None
        for line in process.stdout:
            sys.stdout.write(line)
            sys.stdout.flush()
            if log:
                log.write(line)
                log.flush()
        return process.wait()
    finally:
        target.unlink(missing_ok=True)
        if log:
            log.close()


if __name__ == "__main__":
    raise SystemExit(main())
