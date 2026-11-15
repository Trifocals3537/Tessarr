import assert from 'node:assert/strict';
import test from 'node:test';
import {githubReader, main, requiredJobs, validateBeta} from './beta-publish-gate.mjs';

const sha = 'a'.repeat(40);
function fixture() {
    const env = {
        GITHUB_EVENT_NAME: 'workflow_dispatch', GITHUB_REPOSITORY: 'Trifocals3537/tessarr',
        GITHUB_REPOSITORY_ID: '1426317456', GITHUB_REF: 'refs/heads/beta', GITHUB_SHA: sha,
        GITHUB_RUN_ATTEMPT: '1', BETA_MODE: 'validate', BETA_EXPECTED_SHA: sha, BETA_CI_RUN_ID: '42',
    };
    const run = {id: 42, workflow_id: 7, path: '.github/workflows/ci.yml',
        repository: {id: 1426317456, full_name: 'Trifocals3537/tessarr'}, head_repository: {id: 1426317456},
        head_branch: 'beta', head_sha: sha, event: 'push', status: 'completed', conclusion: 'success', run_attempt: 1};
    const jobs = requiredJobs.map((name, index) => ({id: index + 1, name, run_id: 42,
        run_attempt: 1, head_sha: sha, status: 'completed', conclusion: 'success'}));
    const state = {env, run, jobs, current: null, branchReads: 0, runReads: 0, runs: null,
        workflow: {id: 7, path: '.github/workflows/ci.yml', state: 'active'}, moved: false, pages: null, calls: []};
    state.read = async path => {
        state.calls.push(path);
        if (path === 'branches/beta') return {commit: {sha: state.moved && state.branchReads++ > 0 ? 'b'.repeat(40) : sha}};
        if (path === 'actions/workflows/ci.yml') return structuredClone(state.workflow);
        if (path === 'actions/runs/42') return structuredClone(state.runReads++ > 0 && state.current ? state.current : state.run);
        if (path.startsWith('actions/runs/42/attempts/')) {
            const page = Number(new URL(`https://example.invalid/${path}`).searchParams.get('page'));
            return {total_count: state.jobs.length, jobs: structuredClone(state.pages ? state.pages[page - 1] ?? [] : state.jobs)};
        }
        if (path.startsWith('actions/workflows/7/runs?')) {
            const runs = state.runs ?? [state.run];
            return {total_count: runs.length, workflow_runs: structuredClone(runs)};
        }
        throw new Error(`Unexpected API request: ${path}`);
    };
    return state;
}

for (const mode of ['validate', 'build', 'publish']) {
    test(`accepts exact current beta with successful complete CI in ${mode} mode`, async () => {
        const state = fixture(); state.env.BETA_MODE = mode;
        assert.deepEqual(await validateBeta(state.env, state.read), {sha, runId: 42, attempt: 1});
    });
}

for (const [key, value] of [
    ['GITHUB_EVENT_NAME', 'push'], ['GITHUB_REPOSITORY', 'fork/tessarr'], ['GITHUB_REPOSITORY_ID', '1'],
    ['GITHUB_REF', 'refs/heads/main'], ['GITHUB_REF', 'refs/tags/beta'], ['GITHUB_SHA', 'b'.repeat(40)],
    ['BETA_EXPECTED_SHA', 'a'.repeat(7)], ['BETA_EXPECTED_SHA', `${sha}\nsha=bad`],
    ['BETA_EXPECTED_SHA', ''], ['BETA_CI_RUN_ID', ''], ['BETA_CI_RUN_ID', '42; echo bad'],
    ['BETA_CI_RUN_ID', '0'], ['BETA_CI_RUN_ID', '9007199254740992'], ['BETA_MODE', ''], ['BETA_MODE', 'automatic'],
]) {
    test(`rejects invalid dispatch ${key}=${JSON.stringify(value)} before API reads`, async () => {
        const state = fixture(); state.env[key] = value;
        await assert.rejects(validateBeta(state.env, state.read));
        assert.equal(state.calls.length, 0);
    });
}

test('publishing cannot be repeated by rerunning an old request', async () => {
    const state = fixture(); state.env.BETA_MODE = 'publish'; state.env.GITHUB_RUN_ATTEMPT = '2';
    await assert.rejects(validateBeta(state.env, state.read), /new explicit request/);
    assert.equal(state.calls.length, 0);
    state.env.BETA_MODE = 'validate';
    await validateBeta(state.env, state.read);
});

for (const [key, value] of [
    ['id', 41], ['workflow_id', 9], ['path', '.github/workflows/release.yml'],
    ['repository', {id: 1}], ['head_repository', {id: 1}], ['head_branch', 'main'], ['head_sha', 'b'.repeat(40)],
    ['event', 'pull_request'], ['event', 'pull_request_target'], ['status', 'in_progress'],
    ['conclusion', 'failure'], ['conclusion', 'skipped'], ['conclusion', 'cancelled'], ['run_attempt', 0],
]) {
    test(`rejects CI mismatch ${key}=${JSON.stringify(value)}`, async () => {
        const state = fixture(); state.run[key] = value;
        await assert.rejects(validateBeta(state.env, state.read));
    });
}

for (const [key, value] of [['id', 0], ['path', '.github/workflows/other.yml'], ['state', 'disabled_manually']]) {
    test(`rejects unavailable CI workflow ${key}`, async () => {
        const state = fixture(); state.workflow[key] = value;
        await assert.rejects(validateBeta(state.env, state.read));
    });
}

for (const name of requiredJobs) {
    for (const conclusion of ['failure', 'skipped', 'cancelled', null]) {
        test(`requires ${name} to succeed (${conclusion})`, async () => {
            const state = fixture(); state.jobs.find(job => job.name === name).conclusion = conclusion;
            await assert.rejects(validateBeta(state.env, state.read));
        });
    }
    test(`requires exactly one ${name}`, async () => {
        const state = fixture(); state.jobs = state.jobs.filter(job => job.name !== name);
        await assert.rejects(validateBeta(state.env, state.read), /required CI job/);
        const duplicate = fixture();
        duplicate.jobs.push({...duplicate.jobs.find(job => job.name === name), id: 100});
        await assert.rejects(validateBeta(duplicate.env, duplicate.read), /required CI job/);
    });
}

test('uses the current attempt and reads every job page', async () => {
    const state = fixture(); state.run.run_attempt = 3;
    state.jobs = state.jobs.map(job => ({...job, run_attempt: 3}));
    state.pages = [state.jobs.slice(0, 2), state.jobs.slice(2)];
    await validateBeta(state.env, state.read);
    assert(state.calls.includes('actions/runs/42/attempts/3/jobs?per_page=100&page=2'));
});

test('does not hide a failed job on the second page', async () => {
    const state = fixture(); state.jobs.at(-1).conclusion = 'failure';
    state.pages = [state.jobs.slice(0, 2), state.jobs.slice(2)];
    await assert.rejects(validateBeta(state.env, state.read), /Every job/);
});

test('an extra skipped CI job cannot be hidden by five successful jobs', async () => {
    const state = fixture();
    state.jobs.push({...state.jobs[0], id: 100, name: 'Additional policy', conclusion: 'skipped'});
    await assert.rejects(validateBeta(state.env, state.read), /Every job/);
});

for (const mutate of [
    state => { state.moved = true; },
    state => { state.read = async path => path === 'branches/beta' ? {commit: {sha: 'b'.repeat(40)}} : {}; },
    state => { state.current = {...state.run, status: 'in_progress', conclusion: null, run_attempt: 2}; },
    state => { state.current = {...state.run, run_attempt: 2}; },
    state => { state.runs = [state.run, {...state.run, id: 43, conclusion: 'failure'}]; },
    state => { state.runs = [state.run, {...state.run, id: 43, status: 'queued', conclusion: null}]; },
    state => { state.jobs[0].run_attempt = 2; },
    state => { state.jobs[0].run_id = 41; },
    state => { state.jobs[0].head_sha = 'b'.repeat(40); },
    state => { state.jobs[0].status = 'in_progress'; },
    state => { state.pages = [state.jobs.slice(0, 2), []]; },
    state => { state.pages = [state.jobs.slice(0, 2), state.jobs.slice(0, 3)]; },
]) {
    test(`rejects changing or incomplete evidence: ${mutate.toString()}`, async () => {
        const state = fixture(); mutate(state);
        await assert.rejects(validateBeta(state.env, state.read));
    });
}

test('API reader fails closed and never follows a token-bearing redirect', async () => {
    const read = githubReader('synthetic-token', async (url, options) => {
        assert.equal(url, 'https://api.github.com/repos/Trifocals3537/tessarr/branches/beta');
        assert.equal(options.redirect, 'error');
        assert.equal(options.headers.Authorization, 'Bearer synthetic-token');
        return {ok: false, status: 403};
    });
    await assert.rejects(read('branches/beta'), /403/);
    await assert.rejects(githubReader('synthetic', async () => ({ok: true, json: async () => []}))('branches/beta'), /Invalid GitHub response/);
    await assert.rejects(githubReader('synthetic', async () => { throw new Error('network unavailable'); })('branches/beta'));
});

test('returns validated evidence only after successful validation', async () => {
    const state = fixture();
    state.env.BETA_EXPECTED_SHA = 'invalid';
    await assert.rejects(main(state.env, state.read));
    state.env.BETA_EXPECTED_SHA = sha;
    assert.deepEqual(await main(state.env, state.read), {sha, runId: 42, attempt: 1});
});
