const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

// Smoke coverage for the React console. The login POST sets the session cookie; instead of
// racing the redirect we navigate to /ui/app explicitly and assert that the shell renders,
// that the JSON session bootstrap worked (no "会话不可用") and that the runs table is drawn.
test.describe('react console', () => {
  test('the console renders for an authenticated operator', async ({ page }) => {
    await page.goto('/ui/login');
    await page.fill('#token', token);
    await Promise.all([page.waitForLoadState('load'), page.click('button[type=submit]')]);

    await page.goto('/ui/app');
    await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN');
    await expect(page.getByText('账号开通控制台')).toBeVisible({ timeout: 15000 });
    await expect(page.getByRole('heading', { name: '运行' })).toBeVisible({ timeout: 15000 });
    await expect(page.getByRole('link', { name: '创建运行' })).toBeVisible();
    await expect(page.locator('table')).toBeVisible();
    await expect(page.getByText('会话不可用')).toHaveCount(0);
  });
});
