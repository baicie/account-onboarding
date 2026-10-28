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
  await page.selectOption('#filter_template', 'sandbox-email-verify-oidc');
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

test('alerts outbox reconciles durable state and resolves without dispatch', async ({ page }) => {
  await login(page);
  await page.click('nav a[href="/ui/alerts/outbox"]');
  await expect(page.locator('h1')).toContainText('Alerts');
  await expect(page.locator('text=External alert transport is not configured')).toBeVisible();
  await expect(page.locator('form[action="/ui/alerts/outbox/dispatch"] button')).toBeDisabled();
  await page.click('form[action="/ui/alerts/reconcile"] button');
  await expect(page.locator('.alert.notice')).toContainText('reconciled');
  await expect(page.locator('table')).toContainText('backup_verification_missing');
  await page.reload();
  await expect(page.locator('table')).toContainText('backup_verification_missing');
  await page.locator('table a[href^="/ui/alerts/outbox/"]').first().click();
  await expect(page.locator('h2', { hasText: 'Delivery history' })).toBeVisible();
  await page.goBack();
  await page.locator('form[action="/ui/alerts/outbox/resolve"] button').first().click();
  await expect(page.locator('.alert.notice')).toContainText('resolved');
  await page.goto('/ui/alerts/outbox?status=RESOLVED');
  await expect(page.locator('table')).toContainText('backup_verification_missing');
});

test('runs filters remain in URL and empty state is explicit', async ({ page }) => {
  await login(page);
  const status = await page.locator('#filter_status option:not([value=""])').first().getAttribute('value');
  await page.selectOption('#filter_status', status);
  await page.selectOption('#filter_template', 'sandbox-email-verify-oidc');
  await page.click('#run-filters button[type=submit]');
  await expect(page).toHaveURL(/status=/);
  await expect(page).toHaveURL(/template=sandbox-email-verify-oidc/);
  await page.goto('/ui/?status=CANCELLED&template=sandbox-email-verify-oidc');
  await expect(page.locator('td', { hasText: 'No runs yet' })).toBeVisible();
});

test('run detail shows steps and event timeline', async ({ page }) => {
  await login(page);
  await page.locator('table a[href^="/ui/runs/"]').first().click();
  await expect(page.locator('h1')).toContainText('Run');
  await expect(page.locator('h2', { hasText: 'Steps' })).toBeVisible();
  await expect(page.locator('h2', { hasText: 'Event timeline' })).toBeVisible();
  await page.reload();
  await expect(page.locator('h2', { hasText: 'Event timeline' })).toBeVisible();
});
