import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { closeSync, openSync } from 'node:fs';
import { cp, mkdir, mkdtemp, readFile, realpath, rename, writeFile } from 'node:fs/promises';
import net from 'node:net';
import path from 'node:path';
import { createServer as createTLSServer } from 'node:tls';
import { setTimeout as delay } from 'node:timers/promises';
import { crc32 } from 'node:zlib';

const [scratchPath, repoPath, stableExe, betaExe, betaVersion] = process.argv.slice(2);
assert(scratchPath && repoPath && stableExe && betaExe && betaVersion);
const scratch = await realpath(scratchPath);
const certificate = path.join(scratch, 'fixture-tls.pem');
const root = await mkdtemp(path.join(scratch, 'run-'));
const state = path.join(root, 'state');
const settings = path.join(state, 'config');
const downloads = path.join(state, 'downloads');
const checkpoint = path.join(root, 'stable-checkpoint');
const auth = {
  username: 'upgrade-fixture',
  password: (await readFile(path.join(scratch, 'fixture-password.hash'), 'utf8')).trim(),
  api_token: randomBytes(32).toString('hex'),
  session_secret: randomBytes(32).toString('hex'),
  session_version: 1,
};
const password = 'synthetic-upgrade-fixture-password';
const basic = 'Basic ' + Buffer.from(auth.username + ':' + password).toString('base64');
const category = 'upgrade-fixture';
const firstName = 'Upgrade.PAR2.Fixture';
const queuedName = 'Upgrade.Queued.Fixture';
const sockets = new Set();
const articles = new Map();
const missing = new Set();
const requests = new Map();
const blocked = new Set();
const held = new Set();
const stages = [];
let child;
let childDone;
let baseURL;

await mkdir(settings, { recursive: true });
await mkdir(downloads);

function report(line) {
  stages.push(line);
  console.log(line);
}

function yenc(data, filename, start, part, total, size) {
  const body = [];
  let column = 0;
  for (const byte of data) {
    let v = (byte + 42) & 255;
    if (column === 0 && v === 46) body.push(46); // NNTP dot stuffing.
    if ([0, 10, 13, 61].includes(v)) {
      body.push(61);
      v = (v + 64) & 255;
      column++;
    }
    body.push(v);
    column++;
    if (column >= 128) {
      body.push(13, 10);
      column = 0;
    }
  }
  return Buffer.concat([
    Buffer.from('=ybegin part=' + part + ' total=' + total + ' line=128 size=' + size + ' name=' + filename +
      '\r\n=ypart begin=' + (start + 1) + ' end=' + (start + data.length) + '\r\n'),
    Buffer.from(body),
    Buffer.from('\r\n=yend size=' + data.length + ' part=' + part +
      ' pcrc32=' + crc32(data).toString(16).padStart(8, '0') + '\r\n.\r\n'),
  ]);
}

function makeNZB(files, partSize) {
  const xml = ['<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">'];
  for (const [name, bytes] of files) {
    const parts = Math.ceil(bytes.length / partSize);
    xml.push('<file poster="synthetic" date="1700000000" subject="&quot;' + name +
      '&quot; yEnc (1/' + parts + ')"><groups><group>alt.test</group></groups><segments>');
    for (let start = 0, i = 0; start < bytes.length; start += partSize, i++) {
      const id = name + '-' + i + '@upgrade.invalid';
      const data = bytes.subarray(start, start + partSize);
      const body = yenc(data, name, start, i + 1, parts, bytes.length);
      articles.set(id, body);
      xml.push('<segment number="' + (i + 1) + '" bytes="' + (body.length - 3) + '">' + id + '</segment>');
    }
    xml.push('</segments></file>');
  }
  return xml.join('') + '</nzb>';
}

const fixtureFiles = [];
for (const name of ['fileA.bin', 'fileB.bin', 'realtool.par2', 'realtool.vol0+4.par2']) {
  const data = await readFile(path.join(repoPath, 'internal', 'par2', 'testdata', name));
  fixtureFiles.push([name, data]);
}
const original = fixtureFiles[0][1];
const par2NZB = makeNZB(fixtureFiles, 512);
const queuedData = randomBytes(512 * 1024);
const queuedNZB = makeNZB([['queued.bin', queuedData]], 16 * 1024);

const nntp = createTLSServer({
  cert: await readFile(certificate),
  key: await readFile(path.join(scratch, 'fixture-tls.key')),
  minVersion: 'TLSv1.2',
}, socket => {
  sockets.add(socket);
  socket.on('error', () => {});
  socket.on('close', () => sockets.delete(socket));
  socket.setEncoding('ascii');
  socket.write('200 synthetic NNTP fixture\r\n');
  let pending = '';
  socket.on('data', data => {
    pending += data;
    for (let end = pending.indexOf('\n'); end >= 0; end = pending.indexOf('\n')) {
      const [verb, arg] = pending.slice(0, end).trim().split(/\s+/);
      pending = pending.slice(end + 1);
      const id = arg?.replace(/^<|>$/g, '');
      switch (verb.toUpperCase()) {
        case 'AUTHINFO':
          socket.write(arg === 'USER' ? '381 password required\r\n' : '281 authenticated\r\n');
          break;
        case 'MODE':
          socket.write('200 reader ready\r\n');
          break;
        case 'DATE':
          socket.write('111 20261117000000\r\n');
          break;
        case 'STAT':
        case 'BODY':
        case 'ARTICLE':
          requests.set(verb + ' ' + id, (requests.get(verb + ' ' + id) ?? 0) + 1);
          if (verb !== 'STAT' && blocked.has(id)) {
            held.add(id);
            break;
          }
          if (!articles.has(id) || missing.has(id)) {
            socket.write('430 no such article\r\n');
          } else if (verb === 'STAT') {
            socket.write('223 1 <' + id + '>\r\n');
          } else {
            socket.write((verb === 'ARTICLE' ? '220' : '222') + ' 1 <' + id + '> follows\r\n');
            if (verb === 'ARTICLE') socket.write('Message-ID: <' + id + '>\r\n\r\n');
            socket.write(articles.get(id));
          }
          break;
        case 'QUIT':
          socket.end('205 bye\r\n');
          break;
        default:
          socket.write('500 unsupported command\r\n');
      }
    }
  });
});

const reserve = net.createServer();
reserve.listen(0, '127.0.0.1');
await once(reserve, 'listening');
const port = reserve.address().port;
await new Promise(resolve => reserve.close(resolve));
nntp.listen(0, '127.0.0.1');
await once(nntp, 'listening');
baseURL = 'http://127.0.0.1:' + port;

await writeFile(path.join(settings, 'auth.json'), JSON.stringify(auth));
await writeFile(path.join(settings, 'config.json'), JSON.stringify({
  bind_address: '127.0.0.1',
  allowed_client_cidrs: ['127.0.0.1/32'],
  port: String(port),
  log_level: 'debug',
  use_auth: true,
  enable_webdav_auth: true,
  download_folder: downloads,
  categories: [category],
  mount: { type: 'none' },
  default_download_action: 'download',
  min_file_size: '1B',
  allowed_file_types: ['bin'],
  max_active_downloads: 1,
  skip_multi_season: true,
  allow_samples: true,
  refresh_interval: '2s',
  usenet: {
    max_connections: 3,
    read_ahead: '64KB',
    buffer_memory: '64MB',
    processing_timeout: '1m',
    import_availability_sample_percent: 100,
    providers: [{ host: '127.0.0.1', port: nntp.address().port, username: 'fixture-nntp',
      password: 'synthetic-nntp-password', ssl: true, max_connections: 3 }],
  },
}));

function request(url, options = {}) {
  return fetch(baseURL + url, { redirect: 'manual', signal: AbortSignal.timeout(8000), ...options });
}

async function json(url, options = {}, expect = 200) {
  const r = await request(url, options);
  const raw = await r.text();
  assert.equal(r.status, expect, url.split('?')[0] + ' returned ' + r.status + ': ' + raw.slice(0, 300));
  return JSON.parse(raw);
}

async function until(label, test, ms = 40000) {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (child && child.exitCode !== null) throw new Error(label + ': Tessarr exited ' + child.exitCode);
    const result = await test();
    if (result) return result;
    await delay(160);
  }
  throw new Error('Timed out waiting for ' + label);
}

async function start(exe, label, version) {
  const args = ['--config', settings, '--pprof='];
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (/^(TESSARR_|DEBRID_|USENET_|ENABLE_PPROF$|UMASK$)/i.test(key)) delete env[key];
  }
  env.SSL_CERT_FILE = certificate;
  env.SSL_CERT_DIR = path.join(scratch, 'empty-ca-directory');
  execFileSync(exe, args.concat('--check-config'), { cwd: root, env, timeout: 12000, windowsHide: true });
  const log = openSync(path.join(root, label + '.log'), 'w');
  try {
    child = spawn(exe, args, { cwd: root, env, windowsHide: true, stdio: ['ignore', log, log] });
    childDone = once(child, 'exit');
  } finally {
    closeSync(log);
  }
  await until(label + ' readiness', async () => {
    try {
      const r = await request('/ready');
      await r.arrayBuffer();
      return r.ok;
    } catch {
      return false;
    }
  });
  const observed = await json('/version');
  assert.equal(observed.version.replace(/^v/, ''), version);
  const configResult = await request('/api/config', { headers: { Authorization: 'Bearer ' + auth.api_token } });
  assert.equal(configResult.status, 200, label + ' API authentication');
  await configResult.arrayBuffer();
  report(label + ': readiness, version and persisted API authentication verified');
}

async function stop(interrupted = false) {
  if (!child) return;
  child.kill(interrupted ? 'SIGKILL' : 'SIGTERM');
  const stopped = await Promise.race([childDone.then(() => true), delay(12000, false, { ref: false })]);
  if (!stopped) {
    child.kill('SIGKILL');
    await childDone;
  }
  child = undefined;
  childDone = undefined;
  assert(stopped, 'Tessarr did not stop within the rehearsal deadline');
}

function sabParams(mode) {
  return new URLSearchParams({ mode, cat: category, output: 'json', ma_username: auth.username, ma_password: password });
}

async function addNZB(name, source) {
  const data = new FormData();
  data.set('name', new Blob([source], { type: 'application/x-nzb' }), name + '.nzb');
  data.set('action', 'download');
  const result = await json('/sabnzbd/api?' + sabParams('addfile'), { method: 'POST', body: data });
  assert.equal(result.status, true, JSON.stringify(result));
  assert.equal(result.nzo_ids.length, 1);
  return result.nzo_ids[0];
}

async function slots(mode) {
  return (await json('/sabnzbd/api?' + sabParams(mode)))[mode].slots ?? [];
}

async function assertCompleted(id) {
  return until('completed SAB job', async () => {
    const rows = (await slots('history')).filter(row => row.nzo_id === id);
    assert(rows.length <= 1, 'duplicate history rows');
    if (rows[0]?.status === 'Failed') throw new Error('Download failed: ' + JSON.stringify(rows[0]));
    return rows[0]?.status === 'Completed' && rows[0];
  });
}

async function assertRange(title, filename, bytes, from, to) {
  const url = '/webdav/__all__/' + encodeURIComponent(title) + '/' + encodeURIComponent(filename);
  const r = await request(url, { headers: { Authorization: basic, Range: 'bytes=' + from + '-' + to } });
  if (r.status !== 206) throw new Error('WebDAV ' + r.status + ': ' + (await r.text()).slice(0, 350));
  assert.equal(r.headers.get('content-range'), 'bytes ' + from + '-' + to + '/' + bytes.length);
  assert(Buffer.from(await r.arrayBuffer()).equals(bytes.subarray(from, to + 1)), 'WebDAV range bytes differ');
}

const api = { Authorization: 'Bearer ' + auth.api_token };
const getRecovery = () => json('/api/usenet/recovery/jobs', { headers: api });

try {
  await start(stableExe, 'stable', '2.6.4');
  const stableID = await addNZB(firstName, par2NZB);
  const stableRow = await assertCompleted(stableID);
  const title = stableRow.nzb_name ?? stableRow.name ?? firstName;
  await assertRange(title, 'fileA.bin', original, 250, 1000);
  report('v2.6.4 imported and served byte-exact media from a generated NZB');

  const queuedArticle = 'queued.bin-6@upgrade.invalid';
  blocked.add(queuedArticle);
  const queuedID = await addNZB(queuedName, queuedNZB);
  await until('download paused on a fixture article', () => held.has(queuedArticle));
  const active = (await slots('queue')).filter(row => row.nzo_id === queuedID);
  assert.equal(active.length, 1, 'expected one active job before upgrade');
  await stop(true);
  await cp(state, checkpoint, { recursive: true, errorOnExist: true, force: false });
  blocked.clear();
  report('Stopped v2.6.4 during an active file download and copied its persisted state');

  await start(betaExe, 'candidate', betaVersion);
  assert.equal((await assertCompleted(stableID)).nzb_name ?? title, title);
  const resumed = await assertCompleted(queuedID);
  assert.equal((await slots('queue')).some(row => row.nzo_id === queuedID), false);
  const finishedFile = await realpath(path.join(resumed.storage, 'queued.bin'));
  assert(finishedFile.startsWith(downloads + path.sep), 'completed file escaped the fixture download root');
  assert((await readFile(finishedFile)).equals(queuedData), 'resumed download bytes differ');
  report('Candidate resumed the v2.6.4 queue entry once and wrote the expected bytes');
  await assertRange(title, 'fileA.bin', original, original.length - 128, original.length - 1);
  await stop();

  for (const id of ['fileA.bin-0@upgrade.invalid', 'fileA.bin-3@upgrade.invalid', 'fileB.bin-5@upgrade.invalid']) {
    missing.add(id);
  }
  const parityArticle = 'realtool.par2-0@upgrade.invalid';
  blocked.add(parityArticle);
  await start(betaExe, 'par2-start', betaVersion);
  const targets = await json('/api/usenet/recovery/targets?q=Upgrade.PAR2', { headers: api });
  assert(targets.items.some(item => item.nzb_id === stableID && item.file === 'fileA.bin' && item.size === original.length),
    'v2.6.4 import was not found as a recovery target');
  const data = new FormData();
  data.set('nzb_id', stableID);
  data.set('file', 'fileA.bin');
  data.set('source', new Blob([par2NZB]), firstName + '.nzb');
  const recovery = await json('/api/usenet/recovery/jobs', { method: 'POST', headers: api, body: data }, 202);
  await until('manual PAR2 job waiting on parity', async () => held.has(parityArticle) &&
    (await getRecovery()).jobs.some(job => job.id === recovery.id && job.state === 'running'));
  await stop(true);
  blocked.clear();
  await start(betaExe, 'par2-resume', betaVersion);
  const done = await until('recovered media', async () => {
    const job = (await getRecovery()).jobs.find(job => job.id === recovery.id);
    if (job?.state === 'failed') throw new Error('PAR2 failed: ' + JSON.stringify(job));
    return job?.state === 'succeeded' && job;
  });
  assert.equal(done.attempts, 2);
  report('Manual PAR2 resumed after interruption and repaired the legacy import');

  await stop();
  requests.clear();
  await start(betaExe, 'candidate-restart', betaVersion);
  for (const [from, to] of [[0, 511], [250, 2200], [1536, 1536], [original.length - 128, original.length - 1]]) {
    await assertRange(title, 'fileA.bin', original, from, to);
  }
  for (const id of [...missing].filter(value => value.startsWith('fileA.bin-'))) {
    assert.equal(requests.get('BODY ' + id) ?? 0, 0, 'patched article was fetched');
  }
  assert.equal((await getRecovery()).jobs.filter(job => job.id === recovery.id && job.state === 'succeeded').length, 1);
  report('Persisted patches served four exact WebDAV ranges after another restart');
  await stop();

  const checkedRoot = await realpath(root);
  const checkedState = await realpath(state);
  const savedCandidate = path.join(checkedRoot, 'candidate-state');
  assert.equal(checkedState, path.join(checkedRoot, 'state'));
  await rename(checkedState, savedCandidate);
  await cp(checkpoint, state, { recursive: true, errorOnExist: true, force: false });
  missing.clear();
  await start(stableExe, 'rollback', '2.6.4');
  await assertCompleted(stableID);
  await assertCompleted(queuedID);
  await assertRange(title, 'fileA.bin', original, 0, 511);
  report('Restored the pre-upgrade checkpoint and verified v2.6.4 readiness, queue and playback');
  await writeFile(path.join(scratch, 'result.json'), JSON.stringify({ stable: '2.6.4', candidate: betaVersion, stages }, null, 2));
} catch (error) {
  for (const label of ['stable', 'candidate', 'par2-start', 'par2-resume', 'candidate-restart', 'rollback']) {
    const appLog = await readFile(path.join(root, label + '.log'), 'utf8').catch(() => '');
    if (appLog) console.error(label + ' log tail:\n' + appLog.trim().split('\n').slice(-24).join('\n'));
  }
  throw error;
} finally {
  await stop();
  for (const socket of sockets) socket.destroy();
  await new Promise(resolve => nntp.close(resolve));
}
