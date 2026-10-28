const { test, expect, request } = require('@playwright/test');

const token = (process.env.ONBOARD_TEST_ADMIN_TOKEN || '').trim();
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');

// The console is a single-page app, so these tests set data up through the JSON API and then
// drive the real UI. They replace the old matrix that clicked through server-rendered forms.
const api = { Authorization: `Bearer ${token}` };

async function createRun(ctx) {
  const resp = await ctx.post('/v1/runs', {
    headers: { ...api, 'Content-Type': 'application/json', 'Idempotency-Key': `matrix-${Date.now()}-${Math.random()}` },
    data: {
      template: 'sandbox-email-verify-oidc',
      mode: 'sandbox-e2e',
      mailbox_provider: 'fake-mailbox',
      identity_provider: 'fake-identity',
      oauth_provider: 'fake-oauth',
      sandbox_provider: 'fake-sandbox',
    },
  });
  const body = await resp.text();
  if (![200, 201, 202].includes(resp.status())) {
    throw new Error(`create run = ${resp.status()} ${body}`);
  }
  return JSON.parse(body).id;
}

async function login(page) {
  await page.goto('/ui/login');
  await page.fill('#token', token);
  await page.click('button[type=submit]');
  await page.waitForURL(/\/ui\/(\?.*)?$/);
  await expect(page.getByRole('heading', { name: '运行' })).toBeVisible({ timeout: 20000 });
}

test.describe('operator matrix on the console', () => {
  test.beforeEach(({}, testInfo) => testInfo.setTimeout(60000));

  test('the runs list renders what the API returns', async ({ page }) => {
    const ctx = await request.newContext({ baseURL: page.context()._options.baseURL ?? undefined });
    const runId = await createRun(ctx);
    await ctx.dispose();

    await login(page);
    await expect(page.getByRole('cell', { name: runId.slice(0, 8) }).first()).toBeVisible({ timeout: 20000 });
  });

  test('filters reach the API and an impossible filter shows the empty state', async ({ page }) => {
    await login(page);
    await page.goto('/ui/?template=sandbox-email-verify-oidc');
    await expect(page.locator('table')).toBeVisible();
    await page.goto('/ui/?template=no-such-template');
    await expect(page.getByText('暂无运行')).toBeVisible({ timeout: 20000 });
  });

  test('run detail shows steps and the event timeline', async ({ page }) => {
    const ctx = await request.newContext({ baseURL: page.context()._options.baseURL ?? undefined });
    const runId = await createRun(ctx);
    await ctx.dispose();

    await login(page);
    await page.goto(`/ui/runs/${runId}`);
    await expect(page.getByRole('heading', { name: /运行/ })).toBeVisible({ timeout: 20000 });
    await expect(page.locator('table th').filter({ hasText: '步骤' }).first()).toBeVisible();
    await expect(page.getByText('事件时间线').first()).toBeVisible();
    await expect(page.getByRole('button', { name: '恢复运行' })).toBeVisible();
    await expect(page.getByRole('button', { name: '取消运行' })).toBeVisible();
  });

  test('creating a run from the console reaches the run detail', async ({ page }) => {
    await login(page);
    await page.getByRole('link', { name: '创建运行' }).click();
    await expect(page.getByRole('heading', { name: '创建运行' })).toBeVisible({ timeout: 20000 });
    await page.getByRole('button', { name: '创建运行' }).click();
    await expect(page).toHaveURL(/\/ui\/runs\//, { timeout: 30000 });
  });

  test('the alerts outbox demands a rule before acting', async ({ page }) => {
    await login(page);
    await page.goto('/ui/alerts');
    await expect(page.getByRole('heading', { name: '告警与发件箱' })).toBeVisible({ timeout: 20000 });
    await page.getByRole('button', { name: '重新启用' }).click();
    await expect(page.getByText('请填写规则名')).toBeVisible();
  });

  test('external operations screen renders', async ({ page }) => {
    await login(page);
    await page.goto('/ui/ops');
    await expect(page.getByRole('heading', { name: '外部对账' })).toBeVisible({ timeout: 20000 });
    await expect(page.locator('table')).toBeVisible();
  });
});
