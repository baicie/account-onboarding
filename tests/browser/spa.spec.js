const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

// Smoke coverage for the console shell: the bundle loads, the session bootstrap authenticates
// and the runs screen renders its table. Diagnostics print on failure.
test.describe('console shell', () => {
  test('renders for an authenticated operator', async ({ page }) => {
    test.setTimeout(60000);
    const logs = [];
    page.on('pageerror', (e) => logs.push(`[pageerror] ${e.message}`));
    page.on('requestfailed', (r) => logs.push(`[requestfailed] ${r.url()} ${r.failure()?.errorText}`));

    await page.goto('/ui/login');
    await page.fill('#token', token);
    await page.click('button[type=submit]');
    await page.waitForURL(/\/ui\/(\?.*)?$/);
    await page.waitForLoadState('networkidle');

    const probe = await page.evaluate(async () => {
      const res = await fetch('/ui/api/session', { credentials: 'include' });
      return res.status;
    });
    if (probe !== 200) console.log(logs.join('\n'), await page.content());
    expect(probe, 'session bootstrap').toBe(200);

    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN');
    await expect(page.getByRole('heading', { name: '运行' })).toBeVisible({ timeout: 20000 });
    await expect(page.getByRole('link', { name: '创建运行' })).toBeVisible();
    await expect(page.locator('table')).toBeVisible();
  });
});
