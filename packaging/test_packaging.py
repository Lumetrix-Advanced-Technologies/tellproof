"""Tests for the pip and npm packages of the agent (D-054): stub binaries in,
real wheels and npm packages out, installed and run on this machine."""
import base64
import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import build_npm  # noqa: E402
import build_wheels  # noqa: E402

STUB = "#!/bin/sh\necho \"stub agent: $*\"\nexit 3\n"


def go_platform():
    m = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    return {"linux": "linux", "darwin": "darwin"}.get(sys.platform if sys.platform != "linux2" else "linux"), m


class Packaging(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.dist = self.tmp / "dist"
        self.dist.mkdir()
        for goos, goarch in build_wheels.PLATFORMS:
            p = self.dist / f"plumbline-agent-{goos}-{goarch}{'.exe' if goos == 'windows' else ''}"
            p.write_text(STUB if goos != "windows" else "not a real windows binary")
            p.chmod(0o755)
        self.license = self.tmp / "LICENSE"
        self.license.write_text("Apache License\nVersion 2.0, January 2004\n")

    def test_wheels_have_the_right_layout(self):
        out = self.tmp / "pypi"
        self.assertEqual(build_wheels.main(["--dist", str(self.dist), "--version", "v1.2.3", "--out", str(out), "--license", str(self.license)]), 0)
        wheels = sorted(p.name for p in out.glob("*.whl"))
        self.assertEqual(len(wheels), 6, wheels)
        for w in out.glob("*.whl"):
            self.assertTrue(w.name.startswith("plumbline_agent-1.2.3-py3-none-"), w.name)
            with zipfile.ZipFile(w) as z:
                names = z.namelist()
                info = "plumbline_agent-1.2.3.dist-info"
                for need in ["plumbline_agent/__init__.py", "plumbline_agent/__main__.py", f"{info}/METADATA", f"{info}/WHEEL",
                             f"{info}/entry_points.txt", f"{info}/RECORD", f"{info}/LICENSE"]:
                    self.assertIn(need, names, w.name)
                exe = [n for n in names if n.startswith("plumbline_agent/bin/")]
                self.assertEqual(len(exe), 1, names)
                self.assertEqual((z.getinfo(exe[0]).external_attr >> 16) & 0o777, 0o755)
                self.assertEqual(exe[0].endswith(".exe"), "win_" in w.name)
                # RECORD lists every file with its hash and size (pip checks it).
                for line in z.read(f"{info}/RECORD").decode().splitlines():
                    path, digest, size = line.split(",")
                    if path.endswith("RECORD"):
                        continue
                    data = z.read(path)
                    want = "sha256=" + base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
                    self.assertEqual((digest, int(size)), (want, len(data)), path)
                wheel = z.read(f"{info}/WHEEL").decode()
                self.assertIn("Root-Is-Purelib: false", wheel)
                tag = w.name[len("plumbline_agent-1.2.3-py3-none-"):-len(".whl")]
                for t in tag.split("."):
                    self.assertIn(f"Tag: py3-none-{t}", wheel)
                self.assertIn("Version: 1.2.3", z.read(f"{info}/METADATA").decode())
        # Same inputs, same bytes.
        again = self.tmp / "again"
        build_wheels.main(["--dist", str(self.dist), "--version", "1.2.3", "--out", str(again), "--license", str(self.license)])
        for w in out.glob("*.whl"):
            self.assertEqual(w.read_bytes(), (again / w.name).read_bytes(), w.name)

    def test_wheel_installs_and_runs_the_binary(self):
        goos, goarch = go_platform()
        if (goos, goarch) not in build_wheels.PLATFORMS or goos == "windows":
            self.skipTest("no stub for this platform")
        out = self.tmp / "pypi"
        build_wheels.main(["--dist", str(self.dist), "--version", "1.2.3", "--out", str(out), "--license", str(self.license)])
        venv = self.tmp / "venv"
        subprocess.run([sys.executable, "-m", "venv", str(venv)], check=True)
        pip = venv / "bin" / "pip"
        r = subprocess.run([str(pip), "install", "--no-index", "--find-links", str(out), "plumbline-agent==1.2.3"], capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        for cmd in ([str(venv / "bin" / "plumbline-agent"), "run", "--local"], [str(venv / "bin" / "python"), "-m", "plumbline_agent", "run", "--local"]):
            r = subprocess.run(cmd, capture_output=True, text=True)
            self.assertEqual((r.returncode, r.stdout.strip()), (3, "stub agent: run --local"), cmd)

    def test_npm_packages(self):
        out = self.tmp / "npm"
        self.assertEqual(build_npm.main(["--dist", str(self.dist), "--version", "v1.2.3", "--out", str(out), "--license", str(self.license)]), 0)
        main = json.loads((out / "plumbline-agent" / "package.json").read_text())
        self.assertEqual(main["version"], "1.2.3")
        self.assertEqual(main["bin"], {"plumbline-agent": "bin/plumbline-agent.js"})
        # npm's provenance check needs the repository exactly as GitHub names it (case included).
        sdk = json.loads((HERE.parent / "sdk" / "js" / "package.json").read_text()) if (HERE.parent / "sdk" / "js" / "package.json").exists() else None
        self.assertEqual(main["repository"]["url"], "git+https://github.com/Lumetrix-Advanced-Technologies/plumbline.git")
        if sdk:
            self.assertEqual(sdk["repository"]["url"], main["repository"]["url"])
        self.assertNotIn("scripts", main)  # no install scripts
        self.assertEqual(sorted(main["optionalDependencies"]), sorted(f"plumbline-agent-{o}-{c}" for o, c in build_npm.PLATFORMS.values()))
        self.assertTrue(all(v == "1.2.3" for v in main["optionalDependencies"].values()))
        for name in main["optionalDependencies"]:
            pj = json.loads((out / name / "package.json").read_text())
            os_, cpu = name.split("-")[-2:]
            self.assertEqual((pj["os"], pj["cpu"], pj["version"]), ([os_], [cpu], "1.2.3"))
            self.assertEqual(pj["repository"], main["repository"])
            self.assertTrue((out / name / "LICENSE").exists())

        node = shutil.which("node")
        goos, goarch = go_platform()
        if not node or (goos, goarch) not in build_npm.PLATFORMS:
            self.skipTest("node not installed, or no stub for this platform")
        os_, cpu = build_npm.PLATFORMS[(goos, goarch)]
        app = self.tmp / "app"
        nm = app / "node_modules"
        nm.mkdir(parents=True)
        shutil.copytree(out / "plumbline-agent", nm / "plumbline-agent")
        launcher = nm / "plumbline-agent" / "bin" / "plumbline-agent.js"
        # Without the platform package: a clear error, not a crash.
        r = subprocess.run([node, str(launcher), "version"], capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertIn("no binary for", r.stderr)
        shutil.copytree(out / f"plumbline-agent-{os_}-{cpu}", nm / f"plumbline-agent-{os_}-{cpu}")
        r = subprocess.run([node, str(launcher), "run", "--local"], capture_output=True, text=True)
        self.assertEqual((r.returncode, r.stdout.strip()), (3, "stub agent: run --local"), r.stderr)

    def test_missing_binary_fails(self):
        (self.dist / "plumbline-agent-darwin-arm64").unlink()
        self.assertEqual(build_wheels.main(["--dist", str(self.dist), "--version", "1.0.0", "--out", str(self.tmp / "w"), "--license", str(self.license)]), 1)
        self.assertEqual(build_npm.main(["--dist", str(self.dist), "--version", "1.0.0", "--out", str(self.tmp / "n"), "--license", str(self.license)]), 1)


if __name__ == "__main__":
    unittest.main()
