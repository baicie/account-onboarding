// Convert Playwright JSON to a minimal allowlisted evidence schema.
// Deliberately drops messages, stacks, attachments and all request data.
const fs = require('fs');
const path = require('path');
const input = process.argv[2];
const output = process.argv[3] || path.join(__dirname, 'browser-evidence.json');
const raw = JSON.parse(fs.readFileSync(input, 'utf8'));
const tests = [];
function walk(suites) {
  for (const suite of suites || []) {
    for (const spec of suite.specs || []) {
      for (const t of spec.tests || []) {
        for (const r of t.results || []) {
          const errors = (r.errors || []).map(e => /timeout/i.test(e.message || '') ? 'timeout' : 'assertion_or_runtime');
          tests.push({ title: spec.title, status: r.status, duration_ms: r.duration,
            retry: r.retry, error_classes: [...new Set(errors)] });
        }
      }
    }
    walk(suite.suites);
  }
}
walk(raw.suites);
const data = { schema: 1, status: raw.stats && raw.stats.unexpected === 0 ? 'passed' : 'failed',
  expected: raw.stats ? raw.stats.expected : 0, unexpected: raw.stats ? raw.stats.unexpected : 0,
  commit: process.env.GITHUB_SHA || 'local', runner: process.env.RUNNER_NAME || process.env.RUNNER_OS || 'local', tests };
fs.writeFileSync(output, JSON.stringify(data, null, 2) + '\n', { mode: 0o600 });
