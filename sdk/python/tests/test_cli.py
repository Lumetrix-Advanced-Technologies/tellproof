"""The `tellproof` command runs the agent; the SDK stays dependency-free (D-067)."""
import os
import stat
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.join(HERE, "..", "src")
PYPROJECT = os.path.join(HERE, "..", "pyproject.toml")

# Runs the command with the tellproof_agent package hidden, so only the PATH counts.
RUN = ("import sys; sys.path.insert(0, %r); sys.modules['tellproof_agent'] = None; "
       "from tellproof._cli import main; sys.exit(main(sys.argv[1:]))") % SRC


def tellproof(args, path, cwd=None):
    env = dict(os.environ, PATH=path)
    return subprocess.run([sys.executable, "-c", RUN, *args], env=env, cwd=cwd, capture_output=True, text=True, timeout=30)


def fake_agent(d, marker=None):
    fake = os.path.join(d, "tellproof-agent")
    with open(fake, "w") as f:
        f.write('#!/bin/sh\n' + (f'touch {marker}\n' if marker else '') + 'echo "agent: $*"\nexit 7\n')
    os.chmod(fake, os.stat(fake).st_mode | stat.S_IXUSR)


class CLITest(unittest.TestCase):
    def test_without_the_agent_it_says_how_to_install_it(self):
        with tempfile.TemporaryDirectory() as empty:
            r = tellproof(["start"], empty)
        self.assertEqual(r.returncode, 1)
        self.assertIn('pip install "tellproof[agent]"', r.stderr)

    @unittest.skipIf(os.name == "nt", "a shell script stands in for the agent")
    def test_it_runs_the_agent_with_the_same_arguments(self):
        with tempfile.TemporaryDirectory() as d:
            fake_agent(d)
            r = tellproof(["stop", "--force"], d + os.pathsep + os.environ.get("PATH", ""))
        self.assertEqual(r.returncode, 7, r.stderr)
        self.assertEqual(r.stdout.strip(), "agent: stop --force")

    @unittest.skipIf(os.name == "nt", "a shell script stands in for the agent")
    def test_an_agent_in_the_working_directory_is_never_run(self):
        # A cloned repository could carry a tellproof-agent: neither the
        # working directory nor a relative PATH entry may reach it.
        with tempfile.TemporaryDirectory() as repo, tempfile.TemporaryDirectory() as empty:
            marker = os.path.join(empty, "ran")
            fake_agent(repo, marker)
            for path in (".", "", os.pathsep.join(["", empty]), "." + os.pathsep + empty):
                r = tellproof(["start"], path, cwd=repo)
                self.assertEqual(r.returncode, 1, (path, r.stdout))
                self.assertIn("not installed", r.stderr)
            self.assertFalse(os.path.exists(marker), "the planted agent ran")

    def test_importing_the_sdk_does_not_load_the_command(self):
        r = subprocess.run([sys.executable, "-c", "import sys; sys.path.insert(0, %r); import tellproof; "
                            "print('tellproof._cli' in sys.modules)" % SRC], capture_output=True, text=True, timeout=30)
        self.assertEqual(r.stdout.strip(), "False", r.stderr)

    def test_the_only_dependency_is_the_optional_agent(self):
        try:
            import tomllib
        except ImportError:
            self.skipTest("tomllib needs Python 3.11")
        with open(PYPROJECT, "rb") as f:
            project = tomllib.load(f)["project"]
        self.assertEqual(project["dependencies"], [])
        self.assertEqual(list(project["optional-dependencies"]), ["agent"])
        self.assertEqual(len(project["optional-dependencies"]["agent"]), 1)
        self.assertTrue(project["optional-dependencies"]["agent"][0].startswith("tellproof-agent"))
        self.assertEqual(project["scripts"], {"tellproof": "tellproof._cli:main"})


if __name__ == "__main__":
    unittest.main()
