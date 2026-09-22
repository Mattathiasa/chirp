const { test, expect } = require('@playwright/test');
const { ensureSetUp, waitForPeers } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('the command palette opens on the shortcut and finds people', async ({ page }) => {
  await waitForPeers(page);
  const first = await page.locator('.row .name').first().innerText();

  await page.keyboard.press('ControlOrMeta+k');
  const palette = page.locator('.modal.palette');
  await expect(palette).toBeVisible();

  await palette.locator('input').fill(first.slice(0, 4));
  await expect(palette.locator('.pal-row').first()).toContainText(first.slice(0, 4));

  // Enter opens the highlighted result.
  await palette.locator('input').press('Enter');
  await expect(palette).toBeHidden();
  await expect(page.locator('.head h2')).toContainText(first);
});

test('the palette searches message text', async ({ page }) => {
  await waitForPeers(page);
  await page.locator('.row').first().click();
  const needle = `needle${Date.now()}`;
  const composer = page.locator('.composer textarea');
  await composer.fill(`find this ${needle}`);
  await composer.press('Enter');
  await expect(page.locator('.bubble', { hasText: needle })).toBeVisible();

  await page.keyboard.press('ControlOrMeta+k');
  const palette = page.locator('.modal.palette');
  await palette.locator('input').fill(needle);
  await expect(palette.locator('.pal-row', { hasText: needle }).first()).toBeVisible();
});

test('escape closes the palette and focus goes back where it was', async ({ page }) => {
  await waitForPeers(page);
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('.modal.palette')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.locator('.modal.palette')).toHaveCount(0);
});
