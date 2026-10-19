"""The `tellproof` command: runs the Tellproof agent (D-067).

`pip install "tellproof[agent]"` installs the agent with the SDK, and
`tellproof start`, `tellproof stop`, `tellproof init` and the rest run it:
they are the `tellproof-agent` commands under a shorter name. The SDK never
imports this module, so the app is unaffected by it.
"""
import os
import subprocess
import sys

INSTALL = 'pip install "tellproof[agent]"'


def on_path(name):
    """name on the PATH, looked up in absolute directories only. Never the
    working directory: shutil.which and Windows search it first, so a
    tellproof-agent planted in a cloned repository would run."""
    exe = name + ".exe" if os.name == "nt" else name
    for d in os.environ.get("PATH", "").split(os.pathsep):
        if not d or not os.path.isabs(d):
            continue
        path = os.path.join(d, exe)
        if os.path.isfile(path) and os.access(path, os.X_OK):
            return path
    return None


def agent_binary():
    """The agent to run: the bundled one of the tellproof-agent package, else
    tellproof-agent on the PATH, else None."""
    try:
        import tellproof_agent  # the [agent] extra
        path = tellproof_agent.binary()
        if os.path.isfile(path):
            return path
    except Exception:
        pass
    return on_path("tellproof-agent")


def main(argv=None):
    args = sys.argv[1:] if argv is None else list(argv)
    path = agent_binary()
    if path is None:
        sys.stderr.write(
            "tellproof: the Tellproof agent is not installed. Install it with the SDK:\n\n"
            f"    {INSTALL}\n\n"
            "or download it from https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases\n")
        return 1
    # argv[0] "tellproof": the agent then suggests `tellproof ...` commands;
    # TELLPROOF_SDK: `tellproof --version` shows this SDK's version too.
    from . import __version__
    os.environ["TELLPROOF_SDK"] = f"tellproof {__version__} (Python SDK)"
    if os.name == "nt":
        try:
            return subprocess.call(["tellproof", *args], executable=path)
        except KeyboardInterrupt:
            return 130
    os.execv(path, ["tellproof", *args])  # replace this process: signals and exit status go to the agent
    return 1  # not reached


if __name__ == "__main__":
    sys.exit(main())
