import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync, spawn } from 'node:child_process';
import { chmod, mkdir, mkdtemp, readFile, realpath, unlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const { RELEASE_ASSET, RELEASE_SHA256 } = process.env;
assert.match(RELEASE_ASSET, /^tessarr_(Linux|Darwin|Windows)_(x86_64|arm64)\.(tar\.gz|zip)$/);
assert.match(RELEASE_SHA256, /^[a-f0-9]{64}$/);
const root = await realpath(await mkdtemp(path.join(tmpdir(), 'tessarr-v270-')));
const response = await fetch(
  `https://github.com/Trifocals3537/Tessarr/releases/download/v2.7.0/${RELEASE_ASSET}`,
  { signal: AbortSignal.timeout(90000) },
);
assert.equal(response.status, 200);
const bytes = Buffer.from(await response.arrayBuffer());
assert.equal(createHash('sha256').update(bytes).digest('hex'), RELEASE_SHA256);
const archive = path.join(root, RELEASE_ASSET);
await writeFile(archive, bytes);
const binaryName = process.platform === 'win32' ? 'tessarr.exe' : 'tessarr';
const expected = [binaryName, 'LICENSE'];
if (process.platform === 'linux') expected.push('tessarr.service');
const entries = execFileSync('tar', ['-tf', archive], { encoding: 'utf8' }).trim().split(/\r?\n/);
assert.deepEqual(entries.sort(), expected.sort());
const listing = execFileSync('tar', ['-tvf', archive], { encoding: 'utf8' }).trim().split(/\r?\n/);
assert.ok(listing.every(line => line.startsWith('-')), 'Package contains a non-regular file');
execFileSync('tar', ['-xf', archive, '-C', root]);
const binary = path.join(root, binaryName);
if (process.platform !== 'win32') await chmod(binary, 0o755);
execFileSync(binary, ['--help'], { timeout: 15000, stdio: 'pipe' });

const configRoot = path.join(root, 'app');
await mkdir(configRoot);
const configPath = path.join(configRoot, 'config.json');
const configBytes = JSON.stringify({
  bind_address: '127.0.0.1',
  port: '18282',
  download_folder: path.join(root, 'downloads'),
  disable_webdav: true,
  mount: { type: 'none' },
  usenet: {
    max_connections: 1,
    processing_max_connections: 1,
    providers: [{
      host: '127.0.0.1', port: 119, username: 'fixture', password: 'fixture', max_connections: 1,
    }],
  },
});
await writeFile(configPath, configBytes);
const environment = { ...process.env, TESSARR_BIND_ADDRESS: '127.0.0.1' };
const check = execFileSync(binary, ['--config', configRoot, '--check-config'], {
  env: environment, encoding: 'utf8', timeout: 15000,
});
assert.ok(check.includes('Tessarr configuration is valid'));
assert.equal(await readFile(configPath, 'utf8'), configBytes);
const authPath = path.join(configRoot, 'auth.json');
await writeFile(authPath, '{malformed');
assert.throws(() => execFileSync(binary, ['--config', configRoot, '--check-config'], {
  env: environment, timeout: 15000, stdio: 'pipe',
}), error => error.status === 1, 'Malformed authentication should be rejected');
await unlink(authPath);

const child = spawn(binary, ['--config', configRoot], { env: environment, stdio: ['ignore', 'pipe', 'pipe'] });
let logs = '';
child.stdout.on('data', chunk => { logs = (logs + chunk).slice(-65536); });
child.stderr.on('data', chunk => { logs = (logs + chunk).slice(-65536); });
const exited = new Promise((resolve, reject) => {
  child.once('error', reject);
  child.once('exit', (code, signal) => resolve({ code, signal }));
});
const requestJson = async endpoint => {
  const result = await fetch(`http://127.0.0.1:18282/${endpoint}`, { signal: AbortSignal.timeout(2000) });
  assert.equal(result.status, 200, `HTTP ${endpoint} failed`);
  return result.json();
};

try {
  let ready = false;
  for (let attempt = 0; attempt < 60; attempt++) {
    if (child.exitCode !== null || child.signalCode !== null) break;
    try {
      const status = await requestJson('ready');
      ready = status.ready && status.phase === 'ready';
    } catch {
      // Startup publishes readiness only after the manager finishes initialization.
    }
    if (ready) break;
    await delay(500);
  }
  assert.ok(ready, `Published package failed to become ready:\n${logs}`);
  assert.deepEqual(await requestJson('live'), { status: 'live' });
  assert.deepEqual(await requestJson('version'), { version: '2.7.0', channel: 'stable' });
  child.kill('SIGTERM');
  const stopped = await Promise.race([
    exited,
    delay(45000, undefined, { ref: false }).then(() => { throw new Error('Published package did not stop'); }),
  ]);
  if (process.platform !== 'win32') {
    assert.equal(stopped.code, 0, logs);
    assert.ok(logs.includes('Tessarr has been stopped gracefully.'), logs);
  }
} finally {
  if (child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL');
    await exited;
  }
}
console.log(`Verified published ${RELEASE_ASSET}: SHA-256, CLI, configuration, invalid auth, readiness, liveness and stable version.`);
if (process.platform !== 'win32') console.log('Graceful shutdown passed.');
