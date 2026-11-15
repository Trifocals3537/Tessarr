import assert from 'node:assert/strict';
import test from 'node:test';
import {execFileSync} from 'node:child_process';
import {existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {delimiter, join} from 'node:path';
import {fileURLToPath} from 'node:url';

// The publisher is Linux-only. CI executes these real shell control paths with
// fake node/skopeo commands; no test authenticates to or writes to a registry.
function runPublisher(t, overrides = {}, args = []) {
    const root = mkdtempSync(join(tmpdir(), 'tessarr-beta-publisher-'));
    t.after(() => rmSync(root, {recursive: true, force: true}));
    const bin = join(root, 'bin'); mkdirSync(bin);
    writeFileSync(join(bin, 'node'), `#!/usr/bin/env bash
set -eu
count=0
if [[ -f "$COUNTER" ]]; then count=$(cat "$COUNTER"); fi
count=$((count+1))
echo "$count" > "$COUNTER"
echo "gate $count" >> "$TRACE"
[[ "\${FAIL_GATE:-}" != "$count" ]]
`, {mode: 0o755});
    writeFileSync(join(bin, 'skopeo'), `#!/usr/bin/env bash
set -eu
echo "skopeo $*" >> "$TRACE"
case "$1" in
  login) cat >/dev/null; [[ "\${FAIL_LOGIN:-}" != 1 ]] ;;
  copy)
    count=0
    if [[ -f "$COPIES" ]]; then count=$(cat "$COPIES"); fi
    count=$((count+1)); echo "$count" > "$COPIES"
    [[ "\${FAIL_COPY:-}" != "$count" ]] ;;
  logout) exit 0 ;;
  *) exit 99 ;;
esac
`, {mode: 0o755});
    const archive = join(root, 'image.tar'); writeFileSync(archive, 'synthetic OCI placeholder');
    const trace = join(root, 'trace');
    const env = {...process.env, PATH: `${bin}${delimiter}${process.env.PATH}`, BETA_MODE: 'publish',
        GH_TOKEN: 'synthetic-test-token', GITHUB_ACTOR: 'synthetic-operator', TRACE: trace,
        COUNTER: join(root, 'counter'), COPIES: join(root, 'copies'), ...overrides};
    let failed = false;
    try {
        execFileSync('bash', [fileURLToPath(new URL('./publish-beta-image.sh', import.meta.url)),
            archive, '2.6.0-beta.42.abcdef0', 'beta', ...args], {env, cwd: root, stdio: 'pipe'});
    } catch { failed = true; }
    return {failed, calls: existsSync(trace) ? readFileSync(trace, 'utf8').trim().split('\n') : []};
}

test('publisher verifies before upload and channel promotion, preserving all manifests', {skip: process.platform === 'win32'}, t => {
    const result = runPublisher(t);
    assert.equal(result.failed, false);
    assert.equal(result.calls.length, 6);
    assert.equal(result.calls[0], 'gate 1');
    assert.match(result.calls[1], /^skopeo login --username synthetic-operator --password-stdin ghcr.io$/);
    assert.match(result.calls[2], /^skopeo copy --all --preserve-digests oci-archive:.* docker:\/\/ghcr.io\/trifocals3537\/tessarr:2.6.0-beta.42.abcdef0$/);
    assert.equal(result.calls[3], 'gate 2');
    assert.equal(result.calls[4], 'skopeo copy --all --preserve-digests docker://ghcr.io/trifocals3537/tessarr:2.6.0-beta.42.abcdef0 docker://ghcr.io/trifocals3537/tessarr:beta');
    assert.equal(result.calls[5], 'skopeo logout ghcr.io');
    assert(!result.calls.join('\n').includes('synthetic-test-token'));
});

for (const [overrides, expectedCopies] of [
    [{FAIL_GATE: '1'}, 0], [{FAIL_LOGIN: '1'}, 0], [{FAIL_COPY: '1'}, 1], [{FAIL_GATE: '2'}, 1],
    [{BETA_MODE: 'validate'}, 0], [{BETA_MODE: ''}, 0],
]) {
    test(`publisher fails closed: ${JSON.stringify(overrides)}`, {skip: process.platform === 'win32'}, t => {
        const result = runPublisher(t, overrides);
        assert.equal(result.failed, true);
        assert.equal(result.calls.filter(call => call.startsWith('skopeo copy')).length, expectedCopies);
        assert(!result.calls.some(call => call.endsWith('tessarr:beta')));
        if (overrides.FAIL_COPY || overrides.FAIL_GATE === '2') assert.equal(result.calls.at(-1), 'skopeo logout ghcr.io');
    });
}
