#!/usr/bin/env python3
"""Build the `plumbline-agent` wheels from release binaries (D-054).

One wheel per platform, each holding that platform's static Go binary and a
small launcher, so `pip install plumbline-agent` puts `plumbline-agent` on the
PATH with nothing downloaded at install time and no build step. Standard
library only: a wheel is a zip with a fixed layout (PEP 427).

    python3 packaging/build_wheels.py --dist dist --version 0.2.0 --out pkg/pypi
"""
import argparse
import base64
import hashlib
import os
import sys
import zipfile
from pathlib import Path

NAME = "plumbline_agent"
DIST = "plumbline-agent"

# Go platform -> wheel platform tag. Go 1.26 binaries need macOS 12 or later;
# the Linux binaries are static, so any glibc or musl system runs them.
PLATFORMS = {
    ("linux", "amd64"): "manylinux_2_17_x86_64.manylinux2014_x86_64.musllinux_1_1_x86_64",
    ("linux", "arm64"): "manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64",
    ("darwin", "amd64"): "macosx_12_0_x86_64",
    ("darwin", "arm64"): "macosx_12_0_arm64",
    ("windows", "amd64"): "win_amd64",
    ("windows", "arm64"): "win_arm64",
}

LAUNCHER = '''"""The Plumbline evidence agent, installed with pip.

This package holds the agent's static binary for this platform; the
`plumbline-agent` command runs it with the same arguments. Source, docs and
signed release binaries: https://github.com/lumetrix-advanced-technologies/plumbline
"""
import os
import subprocess
import sys

__version__ = "{version}"


def binary() -> str:
    """The path of the bundled agent binary."""
    name = "plumbline-agent.exe" if os.name == "nt" else "plumbline-agent"
    return os.path.join(os.path.dirname(os.path.abspath(__file__)), "bin", name)


def main() -> None:
    path = binary()
    args = [path, *sys.argv[1:]]
    if os.name == "nt":
        try:
            sys.exit(subprocess.call(args))
        except KeyboardInterrupt:
            sys.exit(130)
    os.execv(path, args)  # replace this process: signals and exit status go straight to the agent
'''

MAIN = "from . import main\n\nmain()\n"

DESCRIPTION = """# plumbline-agent

The [Plumbline](https://github.com/lumetrix-advanced-technologies/plumbline) evidence agent, installed with pip.

```bash
pip install plumbline-agent plumbline-sdk
plumbline-agent run --local      # no account or server; dashboard on http://127.0.0.1:4319
```

Add `import plumbline; plumbline.init()` to your app. The agent keeps a signed,
content-free hourly record of every model call: models and regions, tokens,
guardrail decisions, personal-data redaction, agent tool use, prompt and model
changes. It never reads a prompt or a completion.

This package carries the same static binary as the GitHub release for your
platform. Apache License 2.0.
"""


def record_hash(data: bytes) -> str:
    return "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()


def build(binary: Path, goos: str, goarch: str, version: str, out: Path, license_text: bytes) -> Path:
    tag = PLATFORMS[(goos, goarch)]
    info = f"{NAME}-{version}.dist-info"
    exe = "plumbline-agent.exe" if goos == "windows" else "plumbline-agent"
    metadata = "\n".join([
        "Metadata-Version: 2.1",
        f"Name: {DIST}",
        f"Version: {version}",
        "Summary: The Plumbline evidence agent: a signed, content-free record of how your AI system runs, with a local dashboard.",
        "Home-page: https://github.com/lumetrix-advanced-technologies/plumbline",
        "Author: Lumetrix",
        "License: Apache-2.0",
        "Project-URL: Source, https://github.com/lumetrix-advanced-technologies/plumbline",
        "Project-URL: Releases, https://github.com/lumetrix-advanced-technologies/plumbline/releases",
        "Classifier: License :: OSI Approved :: Apache Software License",
        "Classifier: Programming Language :: Python :: 3",
        "Requires-Python: >=3.8",
        "Description-Content-Type: text/markdown",
        "",
        DESCRIPTION,
    ]).encode()
    wheel_meta = "\n".join(["Wheel-Version: 1.0", "Generator: plumbline build_wheels.py", "Root-Is-Purelib: false",
                            *[f"Tag: py3-none-{t}" for t in tag.split(".")], ""]).encode()
    files = [
        (f"{NAME}/__init__.py", LAUNCHER.format(version=version).encode(), 0o644),
        (f"{NAME}/__main__.py", MAIN.encode(), 0o644),
        (f"{NAME}/bin/{exe}", binary.read_bytes(), 0o755),
        (f"{info}/METADATA", metadata, 0o644),
        (f"{info}/WHEEL", wheel_meta, 0o644),
        (f"{info}/entry_points.txt", b"[console_scripts]\nplumbline-agent = plumbline_agent:main\n", 0o644),
        (f"{info}/LICENSE", license_text, 0o644),
    ]
    record = "".join(f"{p},{record_hash(d)},{len(d)}\n" for p, d, _ in files) + f"{info}/RECORD,,\n"
    files.append((f"{info}/RECORD", record.encode(), 0o644))
    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{NAME}-{version}-py3-none-{tag}.whl"
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as z:
        for p, d, mode in files:
            zi = zipfile.ZipInfo(p, date_time=(2026, 1, 1, 0, 0, 0))  # reproducible: same inputs, same wheel
            zi.external_attr = (0o100000 | mode) << 16
            zi.compress_type = zipfile.ZIP_DEFLATED
            z.writestr(zi, d)
    return path


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dist", required=True, help="directory with plumbline-agent-<os>-<arch>[.exe] binaries")
    ap.add_argument("--version", required=True, help="the release version without the v, e.g. 0.2.0")
    ap.add_argument("--out", required=True)
    ap.add_argument("--license", default="LICENSE")
    a = ap.parse_args(argv)
    if a.version.startswith("v"):
        a.version = a.version[1:]
    lic = Path(a.license).read_bytes()
    built = 0
    for (goos, goarch) in PLATFORMS:
        b = Path(a.dist) / f"plumbline-agent-{goos}-{goarch}{'.exe' if goos == 'windows' else ''}"
        if not b.exists():
            print(f"build_wheels: missing {b}", file=sys.stderr)
            return 1
        print(build(b, goos, goarch, a.version, Path(a.out), lic))
        built += 1
    return 0 if built == len(PLATFORMS) else 1


if __name__ == "__main__":
    sys.exit(main())
