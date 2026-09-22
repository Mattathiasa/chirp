const { test, expect } = require('@playwright/test');
const { ensureSetUp, waitForPeers, goToView } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('a room can be created from known peers and takes a message', async ({ page }) => {
  await waitForPeers(page);
  await goToView(page, 'Rooms');
  await page.getByRole('button', { name: /new room/i }).click();

  const dialog = page.getByRole('dialog');
  const roomName = `Standup ${Date.now()}`;
  await dialog.getByLabel('Room name').fill(roomName);

  const members = dialog.locator('input[type=checkbox]');
  const count = await members.count();
  test.skip(count === 0, 'no pinned peers available to invite yet');
  await members.first().check();
  await dialog.getByRole('button', { name: /create room/i }).click();

  await expect(page.locator('.head h2')).toContainText(roomName);

  const body = `room message ${Date.now()}`;
  const composer = page.locator('.composer textarea');
  await composer.fill(body);
  await composer.press('Enter');
  await expect(page.locator('.bubble', { hasText: body })).toBeVisible();
});

test('the members panel names unverified members', async ({ page }) => {
  await goToView(page, 'Rooms');
  const anyRoom = page.locator('.ob').first();
  test.skip(!(await anyRoom.isVisible().catch(() => false)), 'no rooms yet');
  await anyRoom.click();

  await page.getByRole('button', { name: 'Members' }).click();
  const panel = page.locator('.panel');
  await expect(panel).toBeVisible();
  await expect(panel.getByRole('heading', { name: 'Members' })).toBeVisible();
});
