#!/usr/bin/env node
// The `tellproof` command: runs the Tellproof agent (D-067). `npm install
// tellproof` brings the agent as an optional dependency (tellproof-agent and
// its binary for this platform), and `npx tellproof start`, `stop`, `init` and
// the rest run it: they are the tellproof-agent commands under a shorter
// name. The SDK never loads this file, so the app is unaffected by it.
'use strict';
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const { createRequire } = require('node:module');
const path = require('node:path');

const INSTALL = 'npm install tellproof   (without --omit=optional), or npm install tellproof-agent';

// The agent binary of the installed tellproof-agent package, resolved from
// that package so a strict layout (pnpm) finds its platform package too.
function bundledAgent() {
  try {
    const agentPkg = require.resolve('tellproof-agent/package.json');
    const req = createRequire(agentPkg);
    const plat = `tellproof-agent-${process.platform}-${process.arch}`;
    const exe = process.platform === 'win32' ? 'tellproof-agent.exe' : 'tellproof-agent';
    const bin = path.join(path.dirname(req.resolve(`${plat}/package.json`)), 'bin', exe);
    return fs.existsSync(bin) ? bin : null;
  } catch {
    return null;
  }
}

function run(bin) {
  // argv0 "tellproof" and TELLPROOF_COMMAND: the agent then suggests
  // `npx tellproof ...` commands, which is how they run from a project.
  const env = { ...process.env, TELLPROOF_COMMAND: 'npx tellproof' };
  const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit', argv0: 'tellproof', env });
  const forward = (sig) => () => child.kill(sig);
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(sig, forward(sig));
  child.on('error', (err) => {
    console.error(`tellproof: ${err.message}`);
    process.exit(1);
  });
  child.on('exit', (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code ?? 1);
  });
}

// An agent on the PATH (a release binary, pip, Homebrew), looked up in
// absolute directories only. Never the working directory: Windows searches it
// first, so a tellproof-agent planted in a cloned repository would run.
function onPath(name) {
  const exe = process.platform === 'win32' ? `${name}.exe` : name;
  for (const dir of (process.env.PATH || '').split(path.delimiter)) {
    if (!dir || !path.isAbsolute(dir)) continue;
    const file = path.join(dir, exe);
    try {
      fs.accessSync(file, fs.constants.X_OK);
      if (fs.statSync(file).isFile()) return file;
    } catch { /* not here */ }
  }
  return null;
}

const agent = bundledAgent() || onPath('tellproof-agent');
if (agent) {
  run(agent);
} else {
  console.error('tellproof: the Tellproof agent is not installed. Install it with the SDK:\n\n' +
    `    ${INSTALL}\n\n` +
    'or download it from https://github.com/Lumetrix-Advanced-Technologies/tellproof/releases');
  process.exit(1);
}
