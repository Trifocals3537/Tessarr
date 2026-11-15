(function () {
    'use strict';

    function update(summary, documentObject = globalThis.document) {
        summary = summary || {};
        const safeCount = value => {
            const number = Number(value);
            return Number.isFinite(number) && number > 0 ? Math.floor(number) : 0;
        };
        const reasons = ['articles_unavailable', 'invalid_nzb', 'access_denied', 'timeout',
            'provider_unavailable', 'cancelled', 'mixed', 'other_parse_error'];
        const rows = Array.isArray(summary.by_reason) ? summary.by_reason.slice(0, reasons.length) : [];
        documentObject.getElementById('nzb-rejections-total').textContent = safeCount(summary.total_rejected).toLocaleString();
        let visible = 0;
        for (const code of reasons) {
            const row = rows.find(item => item && item.reason === code);
            const count = safeCount(row && row.count);
            let last = '-';
            if (row && typeof row.last_rejected_at === 'string') {
                const date = new Date(row.last_rejected_at);
                if (Number.isFinite(date.getTime())) last = date.toLocaleString();
            }
            documentObject.getElementById(`nzb-count-${code}`).textContent = count.toLocaleString();
            documentObject.getElementById(`nzb-last-${code}`).textContent = last;
            documentObject.getElementById(`nzb-reason-${code}`).classList.toggle('hidden', count === 0);
            if (count > 0) visible++;
        }
        documentObject.getElementById('nzb-rejections-empty').classList.toggle('hidden', visible > 0);
        documentObject.getElementById('nzb-rejections-table').classList.toggle('hidden', visible === 0);
    }

    globalThis.TessarrNZBAdmissions = {update};
})();
