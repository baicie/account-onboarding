const { test, expect } = require('@playwright/test');
const fs = require('fs');

const token = process.env.ONBOARD_TEST_ADMIN_TOKEN;
if (!token) throw new Error('ONBOARD_TEST_ADMIN_TOKEN is required');
const fixturesPath = process.env.ONBOARD_BROWSER_FIXTURES;
const fx = fixturesPath && fs.existsSync(fixturesPath) ? JSON.parse(fs.readFileSync(fixturesPath, 'utf8')) : null;

async function login(page) {
  await page.goto('/ui/login');
  await page.fill('#token', token);
  await Promise.all([page.waitForURL('**/ui/'), page.click('button[type=submit]')]);
}

test.describe('deterministic operator matrix', () => {
  test.beforeEach(async ({}, testInfo) => {
    test.skip(!fx, 'matrix fixtures are not enabled');
    testInfo.setTimeout(45000);
  });

  test('run lookup, filters, pagination and invalid input', async ({ page }) => {
    await login(page);
    await page.fill('#run', fx.run_ids[0]);
    await page.click('form[action="/ui/"] button');
    await expect(page).toHaveURL(new RegExp('/ui/runs/' + fx.run_ids[0]));
    await expect(page.locator('h1')).toContainText(fx.run_ids[0]);
    await page.goto('/ui/');
    await page.selectOption('#filter_status', 'FAILED_TERMINAL');
    await page.selectOption('#filter_template', 'sandbox-email-verify-oidc');
    await page.click('#run-filters button');
    await expect(page).toHaveURL(/status=FAILED_TERMINAL/);
    await expect(page).toHaveURL(/template=sandbox-email-verify-oidc/);
    await expect(page.locator('text=Next page')).toBeVisible();
    await page.click('text=Next page');
    await expect(page).toHaveURL(/cursor=/);
    await expect(page.locator('table tbody tr').first()).toBeVisible();
  });

  test('invalid run filter and cursor return a visible 400 without crash', async ({ page }) => {
    await login(page);
    let resp = await page.goto('/ui/?status=NOPE');
    expect(resp.status()).toBe(400);
    resp = await page.goto('/ui/?cursor=not-base64');
    expect(resp.status()).toBe(400);
    await expect(page.locator('body')).toBeVisible();
  });

  test('external operation filters, detail and audit history', async ({ page }) => {
    await login(page);
    await page.goto('/ui/ops/external?status=OUTCOME_UNKNOWN&provider=cycletls&run_id=' + fx.external_op_run_id);
    await expect(page.locator('table')).toContainText('OUTCOME_UNKNOWN');
    await page.locator('table a[href*="/ui/ops/external/"]').first().click();
    await expect(page.locator('h1')).toContainText('Operation');
    await expect(page.locator('h2', { hasText: 'Audit history' })).toBeVisible();
    await expect(page.locator('text=Last terminal resolution')).toBeVisible();
  });

  test('two independent browser contexts resolve once with one conflict', async ({ browser }) => {
    const baseURL = process.env.ONBOARD_BROWSER_BASE_URL;
    const ctxA = await browser.newContext({ baseURL });
    const ctxB = await browser.newContext({ baseURL });
    try {
      const a = await ctxA.newPage();
      const b = await ctxB.newPage();
      await login(a);
      await login(b);
      const url = '/ui/ops/external/' + fx.external_op_id;
      await a.goto(url);
      await b.goto(url);
      const cookiesA = await ctxA.cookies();
      const cookiesB = await ctxB.cookies();
      expect(cookiesA[0].value).not.toBe(cookiesB[0].value);
      await a.selectOption('select[name="resolution"]', 'RESOURCE_CONFIRMED');
      await a.fill('input[name="reason"]', 'context A verified remote resource');
      await b.selectOption('select[name="resolution"]', 'NO_RESOURCE_CREATED');
      await b.fill('input[name="reason"]', 'context B verified no resource');
      await Promise.all([
        a.waitForURL(/\/ui\/ops\/external(\?|$)/),
        b.waitForURL(/\/ui\/ops\/external(\?|$)/),
        a.click('form[action$="/resolve"] button'),
        b.click('form[action$="/resolve"] button'),
      ]);
      await Promise.all([
        expect(a.locator('.alert')).toBeVisible(),
        expect(b.locator('.alert')).toBeVisible(),
      ]);
      const noticeCount = (await a.locator('.alert.notice').count()) + (await b.locator('.alert.notice').count());
      const errorCount = (await a.locator('.alert.error').count()) + (await b.locator('.alert.error').count());
      expect(noticeCount).toBe(1);
      expect(errorCount).toBe(1);
      const all = (await a.textContent('body')) + (await b.textContent('body'));
      expect(all).toContain('review recorded');
      expect(all).toMatch(/conflict|version/i);
      await a.goto(url);
      await b.goto(url);
      await expect(a.locator('body')).toContainText('RESOLVED');
      await expect(a.locator('form[action$="/resolve"]')).toHaveCount(0);
      await expect(a.locator('text=This operation is not currently resolvable.')).toBeVisible();
      await expect(b.locator('body')).toContainText('RESOLVED');
      const auditRows = a.locator('h2:has-text("Audit history") + table tbody tr');
      await expect(auditRows).toHaveCount(1);
    } finally {
      await ctxA.close();
      await ctxB.close();
    }
  });

  test('alerts filters, delivery history, re-arm and transport fail-closed', async ({ page }) => {
    await login(page);
    await page.goto('/ui/alerts/outbox');
    await expect(page.locator('form[action="/ui/alerts/outbox/dispatch"] button')).toBeDisabled();
    await page.goto('/ui/alerts/outbox?status=DELIVERED');
    await expect(page.locator('table')).toContainText(fx.delivered_rule);
    await page.locator('table a[href^="/ui/alerts/outbox/"]').first().click();
    await expect(page.locator('h2', { hasText: 'Delivery history' })).toBeVisible();
    await expect(page.getByRole('table').last()).toContainText('DELIVERED');
    await page.goto('/ui/alerts/outbox?status=DEAD_LETTER');
    await expect(page.getByRole('table').first()).toContainText(fx.dead_rule);
    page.once('dialog', dialog => dialog.accept());
    await page.locator('form[action="/ui/alerts/outbox/rearm"] button').click();
    await expect(page.locator('.alert.notice')).toContainText('re-armed');
    await page.goto('/ui/alerts/outbox?status=RESOLVED');
    await expect(page.getByRole('table').first()).toContainText(fx.dead_rule);
  });

  test('pending alert resolve closes only active rows and persists', async ({ page }) => {
    await login(page);
    await page.goto('/ui/alerts/outbox?status=PENDING');
    await expect(page.locator('table')).toContainText(fx.pending_rule);
    page.once('dialog', dialog => dialog.accept());
    await page.locator('form[action="/ui/alerts/outbox/resolve"] button').first().click();
    await expect(page.locator('.alert.notice')).toContainText('resolved');
    await page.reload();
    await page.goto('/ui/alerts/outbox?status=RESOLVED');
    await expect(page.locator('table')).toContainText(fx.pending_rule);
  });

  test('create run, reload and logout invalidates the old session', async ({ page }) => {
    await login(page);
    const key = 'r22-matrix-' + Date.now();
    await page.evaluate(k => sessionStorage.setItem('r22-key', k), key);
    await page.fill('#mailbox_provider', 'fake-mailbox');
    await page.fill('#identity_provider', 'fake-identity');
    await page.fill('#oauth_provider', 'fake-oauth');
    await page.click('form[action="/ui/runs"] button[type=submit]');
    await expect(page).toHaveURL(/\/ui\/runs\//);
    await page.reload();
    await expect(page.locator('h1')).toContainText('Run');
    const cookies = await page.context().cookies();
    const session = cookies.find(c => c.name === 'onboard_session');
    expect(session.httpOnly).toBe(true);
    expect(session.sameSite).toBe('Strict');
    await page.click('form[action="/ui/logout"] button');
    await expect(page).toHaveURL(/\/ui\/login/);
    const old = await page.context().addCookies([{ ...session, value: session.value }]);
    void old;
    await page.goto('/ui/');
    await expect(page).toHaveURL(/\/ui\/login/);
  });
});
