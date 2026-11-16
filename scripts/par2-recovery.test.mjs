import assert from 'node:assert/strict';
import test from 'node:test';

globalThis.window = {};
await import('../pkg/server/assets/js/repair.js');

class Element {
    children = [];
    textContent = '';
    disabled = false;
    value = '';
    files = [];
    hidden = false;
    classList = {toggle: (name, hidden) => { this.hidden = hidden; }};
    append(...nodes) { this.children.push(...nodes); }
    replaceChildren(...nodes) { this.children = nodes; }
    addEventListener(name, handler) { this[name] = handler; }
    set innerHTML(value) { throw new Error('Recovery data must render as text'); }
}

function fixture() {
    const elements = new Map(['par2Status', 'par2Jobs', 'par2Search', 'par2SearchBtn', 'par2MoreBtn', 'par2Target', 'par2Source', 'par2StartBtn'].map(id => [id, new Element()]));
    globalThis.document = {getElementById: id => elements.get(id), createElement: () => new Element()};
    const manager = Object.create(window.PAR2RecoveryManager.prototype);
    Object.assign(manager, {api: '/prefix/api', targets: new Map(), cursor: '', query: '', searchRevision: 0, jobsRevision: 0, stopped: false});
    return {manager, elements};
}

test('PAR2 history renders hostile data as text and exposes cancellation only for active jobs', () => {
    const {manager, elements} = fixture();
    manager.renderJobs([{id: 'job-1', file: '<img onerror=alert(1)>', state: 'running', stage: 'Checking', reason: '<script>unsafe</script>', read_bytes: 1048576}, {id: 'job-2', file: 'done.mkv', state: 'succeeded', stage: 'Recovery verified', read_bytes: 0}]);
    const [active, done] = elements.get('par2Jobs').children;
    assert.equal(active.children[0].textContent, '<img onerror=alert(1)>');
    assert.equal(active.children[2].textContent, '<script>unsafe</script>');
    assert.equal(active.children[3].textContent, 'Cancel recovery');
    assert.equal(done.children.length, 2);
});

test('PAR2 search follows bounded pages, respects URLBase and preserves exact target identity', async () => {
    const {manager, elements} = fixture();
    elements.get('par2Search').value = 'Synthetic & test';
    const calls = [];
    manager.request = async path => { calls.push(path); return calls.length === 1 ? {items: [], next: 'cursor'} : {items: [{nzb_id: 'exact-id', nzb: '<release>', file: 'example.mkv'}], next: ''}; };
    await manager.search(true);
    assert.equal(calls.length, 2);
    assert.match(calls[0], /q=Synthetic%20%26%20test/);
    assert.match(calls[1], /after=cursor/);
    const option = elements.get('par2Target').children[0];
    assert.deepEqual(JSON.parse(option.value), ['exact-id', 'example.mkv']);
    assert.equal(option.textContent, '<release> — example.mkv');
});

test('manual PAR2 submission sends selected identity and no invented source URL', async () => {
    const {manager, elements} = fixture();
    const key = JSON.stringify(['nzb-id', 'example.mkv']);
    manager.targets.set(key, {nzb_id: 'nzb-id', file: 'example.mkv'});
    elements.get('par2Target').value = key;
    let sent;
    manager.request = async (path, options) => { sent = {path, options}; return {state: 'queued'}; };
    manager.loadJobs = async () => {};
    await manager.start();
    assert.equal(sent.path, 'jobs');
    assert.equal(sent.options.method, 'POST');
    assert.deepEqual([...sent.options.body.entries()], [['nzb_id', 'nzb-id'], ['file', 'example.mkv']]);
    assert.equal(elements.get('par2StartBtn').disabled, false);
});

test('PAR2 continuation starts a fresh search when the query changes', async () => {
    const {manager, elements} = fixture();
    manager.query = 'old'; manager.cursor = 'old-cursor'; manager.targets.set('old', {file: 'old.mkv'});
    elements.get('par2Search').value = 'new';
    let sent;
    manager.request = async path => { sent = path; return {items: [], next: ''}; };
    await manager.search(false);
    assert.equal(sent, 'targets?q=new&after=');
    assert.equal(manager.targets.size, 0);
});

test('late PAR2 polling responses cannot replace refreshed job status', async () => {
    const {manager, elements} = fixture();
    let finishOld;
    manager.request = () => new Promise(resolve => { finishOld = resolve; });
    const stalePoll = manager.loadJobs();
    manager.request = async () => ({jobs: [{file: 'current.mkv', state: 'succeeded', stage: 'Verified'}]});
    await manager.loadJobs();
    finishOld({jobs: []});
    await stalePoll;
    assert.equal(elements.get('par2Jobs').children[0].children[0].textContent, 'current.mkv');
    assert.notEqual(elements.get('par2Status').textContent, 'No recovery jobs yet.');
});

test('PAR2 submission validates file selection and upload size without starting work', async () => {
    const {manager, elements} = fixture();
    manager.request = async () => { throw new Error('unexpected request'); };
    await manager.start();
    assert.match(elements.get('par2Status').textContent, /select/);
    const key = 'selected'; manager.targets.set(key, {nzb_id: 'id', file: 'file.mkv'}); elements.get('par2Target').value = key;
    elements.get('par2Source').files = [{size: 16 * 1024 * 1024 + 1}];
    await manager.start();
    assert.match(elements.get('par2Status').textContent, /16 MiB/);
});

test('PAR2 API requests keep credentials local and report actionable errors', async () => {
    const {manager} = fixture();
    let sent;
    globalThis.fetch = async (url, options) => { sent = {url, options}; return {ok: false, status: 400, json: async () => ({error: 'Select the original NZB.'})}; };
    await assert.rejects(manager.request('jobs', {method: 'POST'}), /Select the original NZB/);
    assert.equal(sent.url, '/prefix/api/usenet/recovery/jobs');
    assert.equal(sent.options.credentials, 'same-origin');
});

test('PAR2 cancel retains the exact job ID and refreshes status', async () => {
    const {manager} = fixture(); let sent; let refreshed = false;
    manager.request = async (path, options) => { sent = {path, options}; return {}; };
    manager.loadJobs = async () => { refreshed = true; };
    const button = new Element();
    await manager.cancel('id/escaped', button);
    assert.equal(sent.path, 'jobs/id%2Fescaped'); assert.equal(sent.options.method, 'DELETE'); assert.equal(refreshed, true);
});
