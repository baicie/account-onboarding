const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

// Smoke coverage for the React console. On failure it prints the browser console and the
// rendered HTML, because a blank console is otherwise indistinguishable from a slow one.
test.describe('react console', () => {
  test('the console renders for an authenticated operator', async ({ page }) => {
    test.setTimeout(60000);
    const logs = [];
    page.on('console', (m) => logs.push(`[${m.type()}] ${m.text()}`));
    page.on('pageerror', (e) => logs.push(`[pageerror] ${e.message}`));
    page.on('requestfailed', (r) => logs.push(`[requestfailed] ${r.url()} ${r.failure()?.errorText}`));

    await page.goto('/ui/login');
    await page.fill('#token', token);
    await Promise.all([page.waitForLoadState('load'), page.click('button[type=submit]')]);
    await page.goto('/ui/app');
    await page.waitForLoadState('networkidle');

    const html = await page.content();
    const sessionProbe = await page.evaluate(async () => {
      const res = await fetch('/ui/api/session', { credentials: 'include' });
      return { status: res.status, body: (await res.text()).slice(0, 200) };
    });
    if (!html.includes('账号开通控制台') || sessionProbe.status !== 200) {
      console.log('--- console ---');
      console.log(logs.join('\n'));
      console.log('--- session probe ---');
      console.log(JSON.stringify(sessionProbe));
      console.log('--- html ---');
      console.log(html.slice(0, 4000));
    }

    expect(sessionProbe.status, 'session bootstrap must authenticate the console').toBe(200);
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN');
    await expect(page.getByText('账号开通控制台')).toBeVisible({ timeout: 15000 });
    await expect(page.getByRole('heading', { name: '运行' })).toBeVisible({ timeout: 15000 });
    await expect(page.getByRole('link', { name: '创建运行' })).toBeVisible();
    await expect(page.locator('table')).toBeVisible();
    await expect(page.getByText('会话不可用')).toHaveCount(0);
  });
});
