import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const template = readFileSync(new URL('../pkg/server/templates/stats.html', import.meta.url), 'utf8');
const start = template.indexOf('        function updateNZBAdmissionStats(');
const end = template.indexOf('\n        function updateStats(', start);
assert.ok(start >= 0 && end > start, 'Stats must use the tested NZB renderer');
const renderer = template.slice(start, end);

function view() {
    const elements = new Map();
    const element = id => {
        if (!elements.has(id)) {
            const classes = new Set();
            elements.set(id, {textContent: '', classList: {
                toggle(name, enabled) { if (enabled) classes.add(name); else classes.delete(name); },
                contains(name) { return classes.has(name); },
            }});
        }
        return elements.get(id);
    };
    return {elements, element, update(input) {
        vm.runInNewContext(`${renderer}\nupdateNZBAdmissionStats(input);`, {
            input, document: {getElementById: element}, formatNumber: number => number.toLocaleString('en-US'),
        });
    }};
}

test('NZB diagnostics handle startup and absent legacy fields', () => {
    const page = view();
    page.update(undefined);
    assert.equal(page.element('nzb-rejections-total').textContent, '0');
    assert.equal(page.element('nzb-rejections-empty').classList.contains('hidden'), false);
    assert.equal(page.element('nzb-rejections-table').classList.contains('hidden'), true);
});

test('NZB diagnostics display counts and last occurrences by fixed reason', () => {
    const page = view();
    page.update({total_rejected: 3, by_reason: [
        {reason: 'articles_unavailable', count: 2, last_rejected_at: '2026-11-15T12:00:00Z'},
        {reason: 'invalid_nzb', count: 1, last_rejected_at: '2026-11-15T13:00:00Z'},
    ]});
    assert.equal(page.element('nzb-rejections-total').textContent, '3');
    assert.equal(page.element('nzb-count-articles_unavailable').textContent, '2');
    assert.notEqual(page.element('nzb-last-articles_unavailable').textContent, '-');
    assert.equal(page.element('nzb-reason-articles_unavailable').classList.contains('hidden'), false);
    assert.equal(page.element('nzb-reason-timeout').classList.contains('hidden'), true);
    assert.equal(page.element('nzb-rejections-table').classList.contains('hidden'), false);
});

test('NZB diagnostics ignore unknown labels and private error details', () => {
    const page = view();
    const secret = '<img src=x onerror=alert(1)> Secret.Title https://user:password@private.invalid?apikey=private-key';
    page.update({total_rejected: 1, error: secret, by_reason: [
        {reason: 'articles_unavailable', count: 1, last_rejected_at: secret, error: secret},
        {reason: secret, count: 100, last_rejected_at: secret},
        {reason: 'timeout', count: secret},
        {reason: 'invalid_nzb', count: -1},
    ]});
    assert.equal(page.element('nzb-last-articles_unavailable').textContent, '-');
    assert.equal(page.element('nzb-count-timeout').textContent, '0');
    assert.equal(page.element('nzb-count-invalid_nzb').textContent, '0');
    for (const [id, node] of page.elements) {
        assert.equal(id.includes(secret), false);
        assert.equal(String(node.textContent).includes(secret), false);
    }
});

test('NZB diagnostics clear old rows when counters reset', () => {
    const page = view();
    page.update({total_rejected: 1, by_reason: [{reason: 'timeout', count: 1, last_rejected_at: '2026-11-15T12:00:00Z'}]});
    page.update({total_rejected: 0, by_reason: []});
    assert.equal(page.element('nzb-count-timeout').textContent, '0');
    assert.equal(page.element('nzb-last-timeout').textContent, '-');
    assert.equal(page.element('nzb-reason-timeout').classList.contains('hidden'), true);
    assert.equal(page.element('nzb-rejections-empty').classList.contains('hidden'), false);
});

test('NZB diagnostics render every reason in the API contract', () => {
    const page = view();
    const codes = ['articles_unavailable', 'invalid_nzb', 'access_denied', 'timeout',
        'provider_unavailable', 'cancelled', 'mixed', 'other_parse_error'];
    page.update({total_rejected: 8, by_reason: codes.map(reason => ({reason, count: 1}))});
    for (const code of codes) {
        assert.ok(template.includes(`id="nzb-count-${code}"`));
        assert.equal(page.element(`nzb-count-${code}`).textContent, '1');
        assert.equal(page.element(`nzb-reason-${code}`).classList.contains('hidden'), false);
    }
});
