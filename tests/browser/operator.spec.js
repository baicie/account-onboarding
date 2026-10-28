const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

async function login(page) {
  await page.goto('/ui/login');
  await page.fill('#token', token);
  await page.click('button[type=submit]');
  await page.waitForURL(/\/ui\/(\?.*)?$/);
  await expect(page.getByRole('heading', { name: '运行' })).toBeVisible({ timeout: 20000 });
}

test('invalid token never starts a session', async ({ page }) => {
  await page.goto('/ui/login');
  await page.fill('#token', 'wrong-token');
  await page.click('button[type=submit]');
  await expect(page).toHaveURL(/\/ui\/login\?error=/);
  const probe = await page.evaluate(async () => {
    const res = await fetch('/ui/api/session', { credentials: 'include' });
    return res.status;
  });
  expect(probe).toBe(401);
});

test('an anonymous visit is sent to the login page', async ({ page }) => {
  await page.goto('/ui/');
  await expect(page).toHaveURL(/\/ui\/login/, { timeout: 20000 });
});

test('a cookie mutation without the CSRF header is rejected', async ({ page }) => {
  await login(page);
  const resp = await page.request.post('/v1/runs', {
    data: { template: 'sandbox-email-verify-oidc', mode: 'sandbox-e2e' },
    headers: { 'Content-Type': 'application/json' },
  });
  expect([401, 403]).toContain(resp.status());
});

test('signing out revokes the session', async ({ page }) => {
  await login(page);
  await page.getByRole('button', { name: /operator|OP/i }).first().click();
  await page.getByRole('menuitem', { name: '退出登录' }).click();
  await expect(page).toHaveURL(/\/ui\/login/, { timeout: 20000 });
  const probe = await page.evaluate(async () => {
    const res = await fetch('/ui/api/session', { credentials: 'include' });
    return res.status;
  });
  expect(probe).toBe(401);
});
