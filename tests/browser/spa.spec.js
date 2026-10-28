const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

// Smoke coverage for the React console: login lands on /ui/app, the shell renders with the
// Chinese default and the runs screen talks to the JSON API (rather than failing on an
// unauthenticated request).
test.describe('react console', () => {
  test('login lands on the console and the runs screen loads', async ({ page }) => {
    await page.goto('/ui/login');
    await page.fill('#token', token);
    await Promise.all([page.waitForURL(/\/ui\/app/), page.click('button[type=submit]')]);

    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN');
    await expect(page.getByText('账号开通控制台')).toBeVisible();
    await expect(page.getByRole('heading', { name: '运行' })).toBeVisible();
    await expect(page.getByRole('link', { name: '创建运行' })).toBeVisible();
    await expect(page.locator('table')).toBeVisible();

    // The session bootstrap must have returned a CSRF token, otherwise the console shows the
    // "会话不可用" screen instead of the table above.
    await expect(page.getByText('会话不可用')).toHaveCount(0);
  });
});
