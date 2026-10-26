const { test, expect } = require('@playwright/test');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

async function login(page) {
  await page.goto('/ui/login');
  await page.fill('#token', token);
  await Promise.all([page.waitForURL('**/ui/'), page.click('button[type=submit]')]);
}

test('invalid token is rejected without a session', async ({ page }) => {
  await page.goto('/ui/login');
  await page.fill('#token', 'wrong-token');
  await page.click('button[type=submit]');
  await expect(page).toHaveURL(/\/ui\/login/);
  await page.goto('/ui/');
  await expect(page).toHaveURL(/\/ui\/login/);
});

test('admin can create and inspect a real run through the UI', async ({ page }) => {
  await login(page);
  await expect(page.locator('h1')).toContainText('Runs');
  await page.selectOption('#template', 'sandbox-email-verify-oidc');
  await page.fill('#mailbox_provider', 'fake-mailbox');
  await page.fill('#identity_provider', 'fake-identity');
  await page.fill('#oauth_provider', 'fake-oauth');
  await page.locator('form[action="/ui/runs"] button[type=submit]').click();
  await expect(page).toHaveURL(/\/ui\/runs\//);
  await expect(page.locator('h1')).toContainText('Run');
  await page.reload();
  await expect(page.locator('h1')).toContainText('Run');
});

test('cookie mutation without CSRF is rejected', async ({ page, request }) => {
  await login(page);
  const resp = await page.request.post('/ui/runs', { form: { template: 'sandbox-email-verify-oidc' } });
  expect([400, 403]).toContain(resp.status());
});

test('logout clears the admin session', async ({ page }) => {
  await login(page);
  const form = page.locator('form[action="/ui/logout"]');
  await form.locator('button').click();
  await expect(page).toHaveURL(/\/ui\/login/);
  await page.goto('/ui/');
  await expect(page).toHaveURL(/\/ui\/login/);
});
