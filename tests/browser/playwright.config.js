const { defineConfig } = require('@playwright/test');
module.exports = defineConfig({
  testDir: '.',
  timeout: 30000,
  fullyParallel: false,
  reporter: [['line']],
  use: {
    baseURL: process.env.ONBOARD_BROWSER_BASE_URL || 'http://127.0.0.1:8799',
    browserName: 'chromium',
    ...(process.env.PLAYWRIGHT_CHANNEL ? { channel: process.env.PLAYWRIGHT_CHANNEL } : {}),
    headless: true,
    trace: 'off',
    screenshot: 'only-on-failure'
  }
});
