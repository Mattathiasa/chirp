const { test, expect } = require('@playwright/test');
const { ensureSetUp, openWritablePeer, composer, send } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('a message can be sent and is persisted', async ({ page }) => {
  const peer = await openWritablePeer(page);
  const body = `hello ${Date.now()}`;
  await send(page, body);
  await expect(page.locator('.m.me .bubble', { hasText: body })).toBeVisible();

  // It is on disk, not just on screen: reloading brings it back.
  await page.reload();
  await page.locator('.row', { hasText: peer }).first().click();
  await expect(page.locator('.bubble', { hasText: body })).toBeVisible();
});

test('a message can be replied to, and the quote is shown', async ({ page }) => {
  await openWritablePeer(page);
  const first = `quoted ${Date.now()}`;
  await send(page, first);

  const original = page.locator('.m.me', { hasText: first }).first();
  await original.hover();
  await original.getByRole('button', { name: 'Reply' }).click();
  await expect(page.locator('.replying')).toContainText(first);

  const answer = `the answer ${Date.now()}`;
  await send(page, answer);

  const reply = page.locator('.m.me', { hasText: answer }).first();
  await expect(reply.locator('.quote')).toContainText(first);
});

test('a message can be deleted from this device', async ({ page }) => {
  await openWritablePeer(page);
  const body = `delete me ${Date.now()}`;
  await send(page, body);

  const msg = page.locator('.m.me', { hasText: body }).first();
  await msg.hover();
  await msg.getByRole('button', { name: 'Delete' }).click();
  await page.getByRole('button', { name: 'Delete for me', exact: true }).click();

  await expect(page.locator('.bubble', { hasText: body })).toHaveCount(0);
});

test('a message to an offline peer is queued, not lost', async ({ page }) => {
  const peers = await page.request.get('/api/peers').then((r) => r.json());
  const offline = peers.find((p) => !p.online && p.trust !== 'unknown' && p.trust !== 'changed');
  test.skip(!offline, 'no offline bot right now');

  await page.locator('.row', { hasText: offline.name }).first().click();
  const body = `queued ${Date.now()}`;
  await send(page, body);

  const msg = page.locator('.m.me', { hasText: body }).first();
  await expect(msg.locator('.stat')).toContainText(/queued/i);

  // And it shows up in the outbox, which is what actually guarantees delivery.
  const outbox = await page.request.get('/api/outbox').then((r) => r.json());
  expect(outbox.some((m) => m.body === body)).toBe(true);
});
