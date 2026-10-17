#!/usr/bin/env python3
"""Build the `tellproof-agent` npm packages from release binaries (D-054).

The esbuild pattern: one package per platform holds that platform's static
binary (`tellproof-agent-linux-x64`, ...; npm installs only the one whose
`os` and `cpu` match), and the main package, `tellproof-agent`, lists them as
optional dependencies and runs the right one. No install scripts, nothing
downloaded at install time. Standard library only.

    python3 packaging/build_npm.py --dist dist --version 0.2.0 --out pkg/npm
"""
import argparse
import json
import shutil
import sys
from pathlib import Path

# Go platform -> npm (os, cpu).
PLATFORMS = {
    ("linux", "amd64"): ("linux", "x64"),
    ("linux", "arm64"): ("linux", "arm64"),
    ("darwin", "amd64"): ("darwin", "x64"),
    ("darwin", "arm64"): ("darwin", "arm64"),
    ("windows", "amd64"): ("win32", "x64"),
    ("windows", "arm64"): ("win32", "arm64"),
}
# npm checks provenance against this URL, case-sensitively: it must name the
# repository exactly as GitHub does.
REPO = {"type": "git", "url": "git+https://github.com/Lumetrix-Advanced-Technologies/tellproof.git"}
HOME = "https://github.com/lumetrix-advanced-technologies/tellproof"

LAUNCHER = r"""#!/usr/bin/env node
// The Tellproof evidence agent, installed with npm. The platform's binary
// comes in an optional dependency (tellproof-agent-<os>-<cpu>); this runs it
// with the same arguments and exits with its status.
"use strict";
const { spawn } = require("node:child_process");
const path = require("node:path");

const pkg = `tellproof-agent-${process.platform}-${process.arch}`;
const exe = process.platform === "win32" ? "tellproof-agent.exe" : "tellproof-agent";
let bin;
try {
  bin = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", exe);
} catch {
  console.error(`tellproof-agent: no binary for ${process.platform}-${process.arch}. ` +
    `Supported: linux, darwin and win32 on x64 and arm64. If you installed with --omit=optional or --no-optional, ` +
    `install again without it, or download a binary from ${"__HOME__"}/releases.`);
  process.exit(1);
}
const child = spawn(bin, process.argv.slice(2), { stdio: "inherit" });
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(sig, () => child.kill(sig));
child.on("error", (err) => { console.error(`tellproof-agent: ${err.message}`); process.exit(1); });
child.on("exit", (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exit(code ?? 1);
});
"""

README = """# tellproof-agent

The [Tellproof](__HOME__) evidence agent, installed with npm.

```bash
npx tellproof-agent run --local      # no account or server; dashboard on http://127.0.0.1:4319
npm install tellproof            # then: import * as tellproof from 'tellproof'; tellproof.init();
```

The agent keeps a signed, content-free hourly record of every model call: models and regions, tokens, guardrail
decisions, personal-data redaction, agent tool use, prompt and model changes. It never reads a prompt or a completion.
This package runs the same static binary as the GitHub release for your platform. Apache License 2.0.
""".replace("__HOME__", HOME)


def write_json(path: Path, obj) -> None:
    path.write_text(json.dumps(obj, indent=2) + "\n")


def build(dist: Path, version: str, out: Path, license_file: Path) -> list:
    made = []
    optional = {}
    for (goos, goarch), (os_, cpu) in PLATFORMS.items():
        name = f"tellproof-agent-{os_}-{cpu}"
        src = dist / f"tellproof-agent-{goos}-{goarch}{'.exe' if goos == 'windows' else ''}"
        if not src.exists():
            raise FileNotFoundError(f"missing {src}")
        d = out / name
        (d / "bin").mkdir(parents=True, exist_ok=True)
        exe = d / "bin" / ("tellproof-agent.exe" if goos == "windows" else "tellproof-agent")
        shutil.copyfile(src, exe)
        exe.chmod(0o755)
        shutil.copyfile(license_file, d / "LICENSE")
        (d / "README.md").write_text(f"# {name}\n\nThe `tellproof-agent` binary for {os_}-{cpu}. Install `tellproof-agent` instead; it picks this package for you.\n")
        write_json(d / "package.json", {
            "name": name, "version": version,
            "description": f"The Tellproof evidence agent binary for {os_}-{cpu} (used by tellproof-agent).",
            "license": "Apache-2.0", "repository": REPO, "homepage": HOME,
            "os": [os_], "cpu": [cpu], "files": ["bin", "README.md", "LICENSE"], "preferUnplugged": True,
        })
        optional[name] = version
        made.append(d)
    d = out / "tellproof-agent"
    (d / "bin").mkdir(parents=True, exist_ok=True)
    launcher = d / "bin" / "tellproof-agent.js"
    launcher.write_text(LAUNCHER.replace("__HOME__", HOME))
    launcher.chmod(0o755)
    shutil.copyfile(license_file, d / "LICENSE")
    (d / "README.md").write_text(README)
    write_json(d / "package.json", {
        "name": "tellproof-agent", "version": version,
        "description": "The Tellproof evidence agent: a signed, content-free record of how your AI system runs, with a local dashboard.",
        "license": "Apache-2.0", "repository": REPO, "homepage": HOME,
        "keywords": ["llm", "ai", "observability", "opentelemetry", "evidence", "audit", "ai-governance"],
        "bin": {"tellproof-agent": "bin/tellproof-agent.js"},
        "files": ["bin", "README.md", "LICENSE"],
        "engines": {"node": ">=18"},
        "optionalDependencies": optional,
    })
    made.append(d)
    return made


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--dist", required=True)
    ap.add_argument("--version", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--license", default="LICENSE")
    a = ap.parse_args(argv)
    version = a.version[1:] if a.version.startswith("v") else a.version
    try:
        for d in build(Path(a.dist), version, Path(a.out), Path(a.license)):
            print(d)
    except FileNotFoundError as e:
        print(f"build_npm: {e}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
