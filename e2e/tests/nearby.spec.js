const { test, expect } = require('@playwright/test');
const { ensureSetUp, goToView } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('the invite QR is generated and matches the invite URI', async ({ page }) => {
  await goToView(page, 'Nearby');

  // The QR is inline SVG, so it must be in the DOM rather than an <img>.
  const svg = page.locator('.qr svg');
  await expect(svg).toBeVisible();
  await expect(page.locator('.qr img')).toHaveCount(0);

  const invite = await page.request.get('/api/invite').then((r) => r.json());
  expect(invite.uri).toMatch(/^chirp:\/\/add\/[^/]+\/[0-9a-f]{8,}$/);
  await expect(page.locator('.fp', { hasText: 'chirp://add/' })).toContainText(invite.uri);
});

test('a malformed address is rejected rather than dialled', async ({ page }) => {
  await goToView(page, 'Nearby');
  await page.getByLabel('Address or invite link').fill('not-an-address');
  await page.getByRole('button', { name: 'Connect', exact: true }).click();
  await expect(page.locator('.err')).not.toBeEmpty();
});
