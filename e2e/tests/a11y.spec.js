const { test, expect } = require('@playwright/test');
const AxeBuilder = require('@axe-core/playwright').default;
const { ensureSetUp, waitForPeers, goToView } = require('./helpers');

// Zero serious or critical violations is the bar. Anything below that is
// reported but does not fail, so the gate stays meaningful.
async function scan(page) {
  const { violations } = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .analyze();
  const bad = violations.filter((v) => v.impact === 'serious' || v.impact === 'critical');
  if (bad.length) {
    console.log(bad.map((v) => `${v.impact}: ${v.id} — ${v.help}\n  ${v.nodes.map((n) => n.target).join('\n  ')}`).join('\n'));
  }
  return bad;
}

test('the chat view has no serious accessibility violations', async ({ page }) => {
  await ensureSetUp(page);
  await waitForPeers(page);
  await page.locator('.row').first().click();
  expect(await scan(page)).toEqual([]);
});

test('the rooms, nearby and settings views have no serious violations', async ({ page }) => {
  await ensureSetUp(page);
  for (const label of ['Rooms', 'Nearby', 'Settings']) {
    await goToView(page, label);
    expect(await scan(page), `${label} view`).toEqual([]);
  }
});

test('the command palette has no serious violations', async ({ page }) => {
  await ensureSetUp(page);
  await page.keyboard.press('ControlOrMeta+k');
  await expect(page.locator('.modal.palette')).toBeVisible();
  expect(await scan(page)).toEqual([]);
});

test('the page never scrolls sideways', async ({ page }) => {
  await ensureSetUp(page);
  for (const label of ['Rooms', 'Nearby', 'Network', 'Settings']) {
    await goToView(page, label);
    const overflows = await page.evaluate(() =>
      document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);
    expect(overflows, `${label} view scrolls sideways`).toBe(false);
  }
});
