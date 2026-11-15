import {appendFileSync} from 'node:fs';
import {pathToFileURL} from 'node:url';

const repository = 'Trifocals3537/tessarr';
const repositoryId = 1426317456;
const ciPath = '.github/workflows/ci.yml';
export const requiredJobs = [
    'Go tests (windows-latest)', 'Go tests (ubuntu-24.04)',
    'Native Linux build', 'Shared seedbox release candidate', 'Browser assets',
];

function requireThat(condition, message) {
    if (!condition) throw new Error(message);
}

function positiveInteger(value) {
    return Number.isSafeInteger(value) && value > 0;
}

// Request only GitHub's fixed API origin; never follow a redirect with a token.
export function githubReader(token, fetcher = fetch) {
    requireThat(typeof token === 'string' && token.length > 0, 'A GitHub read token is required');
    return async path => {
        const response = await fetcher(`https://api.github.com/repos/${repository}/${path}`, {
            redirect: 'error', signal: AbortSignal.timeout(15000),
            headers: {
                Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json',
                'X-GitHub-Api-Version': '2026-03-10', 'Cache-Control': 'no-cache',
            },
        });
        requireThat(response.ok, `GitHub read failed (${response.status}): ${path}`);
        const value = await response.json();
        requireThat(value !== null && typeof value === 'object' && !Array.isArray(value), `Invalid GitHub response: ${path}`);
        return value;
    };
}

async function listAll(read, path, key) {
    const items = [];
    let total;
    for (let page = 1; page <= 100; page++) {
        const result = await read(`${path}${path.includes('?') ? '&' : '?'}per_page=100&page=${page}`);
        requireThat(Number.isSafeInteger(result.total_count) && result.total_count >= 0 && result.total_count <= 10000, 'Invalid GitHub list count');
        total ??= result.total_count;
        requireThat(total === result.total_count && Array.isArray(result[key]), 'GitHub list changed while being checked');
        requireThat(result[key].length > 0 || total === 0, 'Incomplete GitHub list');
        items.push(...result[key]);
        requireThat(items.length <= total && items.every(item => positiveInteger(item?.id)), 'Invalid GitHub list entries');
        requireThat(new Set(items.map(item => item.id)).size === items.length, 'Duplicate GitHub list entries');
        if (items.length === total) return items;
    }
    throw new Error('GitHub list exceeded pagination limit');
}

function validateRun(run, sha, runId, workflowId) {
    requireThat(run.id === runId && run.workflow_id === workflowId && run.path?.split('@')[0] === ciPath, 'The supplied run is not the CI workflow');
    requireThat(run.repository?.id === repositoryId && run.repository?.full_name === repository && run.head_repository?.id === repositoryId, 'CI belongs to a different repository');
    requireThat(run.head_branch === 'beta' && run.head_sha === sha, 'CI did not test this beta revision');
    requireThat(['push', 'workflow_dispatch'].includes(run.event), 'PR merge tests cannot authorize beta publishing');
    requireThat(run.status === 'completed' && run.conclusion === 'success', 'CI must be completed and successful');
    requireThat(positiveInteger(run.run_attempt), 'CI attempt is invalid');
}

// Also used immediately before registry writes. Reading current CI again
// rejects a newly started/failed rerun of a previously successful attempt.
export async function validateBeta(env, read) {
    requireThat(env.GITHUB_EVENT_NAME === 'workflow_dispatch', 'Beta publishing requires a manual request');
    requireThat(env.GITHUB_REPOSITORY === repository && env.GITHUB_REPOSITORY_ID === String(repositoryId), 'Only the Tessarr repository may publish');
    requireThat(env.GITHUB_REF === 'refs/heads/beta', 'Select the beta branch');
    requireThat(['validate', 'publish'].includes(env.BETA_MODE), 'Select validate or publish mode');
    requireThat(env.BETA_MODE !== 'publish' || env.GITHUB_RUN_ATTEMPT === '1', 'Publishing reruns are refused; make a new explicit request');
    const sha = env.BETA_EXPECTED_SHA;
    requireThat(typeof sha === 'string' && /^[a-f0-9]{40}$/.test(sha) && sha === env.GITHUB_SHA, 'Supply the exact 40-character dispatch revision');
    requireThat(typeof env.BETA_CI_RUN_ID === 'string' && /^[1-9][0-9]*$/.test(env.BETA_CI_RUN_ID), 'Supply a numeric CI run ID');
    const runId = Number(env.BETA_CI_RUN_ID);
    requireThat(positiveInteger(runId), 'CI run ID is outside the supported range');
    const branch = await read('branches/beta');
    requireThat(branch.commit?.sha === sha, 'Beta moved since this request');
    const workflow = await read('actions/workflows/ci.yml');
    requireThat(positiveInteger(workflow.id) && workflow.path === ciPath && workflow.state === 'active', 'The CI workflow is unavailable');
    const runPath = `actions/runs/${runId}`;
    const run = await read(runPath);
    validateRun(run, sha, runId, workflow.id);
    const jobs = await listAll(read, `${runPath}/attempts/${run.run_attempt}/jobs`, 'jobs');
    requireThat(jobs.every(job => job.run_id === runId && job.run_attempt === run.run_attempt && job.head_sha === sha && job.status === 'completed' && job.conclusion === 'success'), 'Every job in the current CI attempt must succeed');
    for (const name of requiredJobs) {
        requireThat(jobs.filter(job => job.name === name).length === 1, `Missing or duplicate required CI job: ${name}`);
    }
    const current = await read(runPath);
    validateRun(current, sha, runId, workflow.id);
    requireThat(current.run_attempt === run.run_attempt, 'CI was rerun during validation');
    const runs = await listAll(read, `actions/workflows/${workflow.id}/runs?branch=beta&head_sha=${sha}`, 'workflow_runs');
    const eligible = runs.filter(candidate => candidate.head_branch === 'beta' && candidate.head_sha === sha && ['push', 'workflow_dispatch'].includes(candidate.event));
    requireThat(eligible.length > 0 && Math.max(...eligible.map(candidate => candidate.id)) === runId, 'Supply the latest CI run for this beta revision');
    validateRun(eligible.find(candidate => candidate.id === runId), sha, runId, workflow.id);
    requireThat((await read('branches/beta')).commit?.sha === sha, 'Beta moved during validation');
    return {sha, runId, attempt: run.run_attempt};
}

export async function main(env = process.env, read = githubReader(env.GH_TOKEN)) {
    const result = await validateBeta(env, read);
    if (env.GITHUB_OUTPUT) appendFileSync(env.GITHUB_OUTPUT, `sha=${result.sha}\nci_run_id=${result.runId}\nci_attempt=${result.attempt}\n`);
    const message = `Validated beta ${result.sha} against CI ${result.runId}, attempt ${result.attempt}. Mode: ${env.BETA_MODE}.`;
    console.log(message);
    if (env.GITHUB_STEP_SUMMARY) appendFileSync(env.GITHUB_STEP_SUMMARY, `${message}\n`);
    return result;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    main().catch(error => { console.error(`Beta gate rejected: ${error.message}`); process.exitCode = 1; });
}
