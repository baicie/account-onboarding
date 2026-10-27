// Sanitized Playwright evidence. Never include request/response bodies, headers,
// cookies, screenshots, traces, stack traces, OAuth codes, or email bodies.
const fs = require('fs');
const path = require('path');

class EvidenceReporter {
  constructor() { this.tests = []; }
  onTestEnd(test, result) {
    const errors = (result.errors || []).map(e => (e.message || '').toLowerCase().includes('timeout') ? 'timeout' : 'assertion_or_runtime');
    this.tests.push({
      title: test.title,
      project: test.parent && test.parent.project ? test.parent.project() : 'chromium',
      status: result.status,
      duration_ms: result.duration,
      retry: result.retry,
      error_classes: [...new Set(errors)]
    });
  }
  onEnd(result) {
    const out = process.env.BROWSER_EVIDENCE_PATH || path.join(__dirname, 'browser-evidence.json');
    const data = {
      schema: 1,
      generated_at: new Date().toISOString(),
      status: result.status,
      expected: result.expectedStatus,
      commit: process.env.GITHUB_SHA || 'local',
      runner: process.env.RUNNER_NAME || process.env.RUNNER_OS || 'local',
      tests: this.tests
    };
    fs.writeFileSync(out, JSON.stringify(data, null, 2) + '\n', { mode: 0o600 });
  }
}
module.exports = EvidenceReporter;
