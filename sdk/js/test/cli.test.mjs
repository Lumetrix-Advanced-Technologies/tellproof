// The `tellproof` command runs the agent; the SDK stays dependency-free (D-067).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = path.dirname(fileURLToPath(import.meta.url));
const bin = path.join(dir, '..', 'bin', 'tellproof.js');
const pkg = JSON.parse(readFileSync(path.join(dir, '..', 'package.json'), 'utf8'));

function tellproof(args, PATH, cwd) {
  return spawnSync(process.execPath, [bin, ...args], { env: { ...process.env, PATH }, cwd, encoding: 'utf8', timeout: 30000 });
}

test('without the agent, tellproof says how to install it', () => {
  const r = tellproof(['start'], mkdtempSync(path.join(tmpdir(), 'tp-empty-')));
  assert.equal(r.status, 1);
  assert.match(r.stderr, /npm install tellproof/);
});

test('tellproof runs the agent with the same arguments', { skip: process.platform === 'win32' }, () => {
  const d = mkdtempSync(path.join(tmpdir(), 'tp-agent-'));
  const fake = path.join(d, 'tellproof-agent');
  writeFileSync(fake, '#!/bin/sh\necho "agent: $*"\nexit 7\n');
  chmodSync(fake, 0o755);
  const r = tellproof(['stop', '--force'], `${d}${path.delimiter}${process.env.PATH}`);
  assert.equal(r.status, 7, r.stderr);
  assert.equal(r.stdout.trim(), 'agent: stop --force');
});

test('an agent in the working directory is never run', { skip: process.platform === 'win32' }, () => {
  // A cloned repository could carry a tellproof-agent: neither the working
  // directory nor a relative PATH entry may reach it.
  const repo = mkdtempSync(path.join(tmpdir(), 'tp-repo-'));
  const marker = path.join(mkdtempSync(path.join(tmpdir(), 'tp-m-')), 'ran');
  writeFileSync(path.join(repo, 'tellproof-agent'), `#!/bin/sh\ntouch ${marker}\nexit 7\n`);
  chmodSync(path.join(repo, 'tellproof-agent'), 0o755);
  for (const PATH of ['.', '', `${path.delimiter}/nonexistent`, `.${path.delimiter}/nonexistent`]) {
    const r = tellproof(['start'], PATH, repo);
    assert.equal(r.status, 1, `PATH=${JSON.stringify(PATH)}: ${r.stdout}`);
    assert.match(r.stderr, /not installed/);
  }
  assert.equal(existsSync(marker), false, 'the planted agent ran');
});

test('tellproof tells the agent its version', { skip: process.platform === 'win32' }, () => {
  const d = mkdtempSync(path.join(tmpdir(), 'tp-ver-'));
  writeFileSync(path.join(d, 'tellproof-agent'), '#!/bin/sh\necho "$TELLPROOF_SDK"\n');
  chmodSync(path.join(d, 'tellproof-agent'), 0o755);
  const r = tellproof(['--version'], d);
  assert.equal(r.stdout.trim(), `tellproof ${pkg.version} (Node.js SDK)`);
});

test('the only dependency is the optional agent, and the SDK never loads the command', () => {
  assert.deepEqual(pkg.dependencies, {});
  assert.deepEqual(Object.keys(pkg.optionalDependencies), ['tellproof-agent']);
  assert.deepEqual(pkg.bin, { tellproof: 'bin/tellproof.js' });
  for (const f of ['index.cjs', 'index.mjs', 'core.cjs', 'export.cjs', 'instrument.cjs', 'otlp.cjs', 'canary.cjs']) {
    assert.doesNotMatch(readFileSync(path.join(dir, '..', 'lib', f), 'utf8'), /bin\/tellproof|tellproof-agent/, f);
  }
});
