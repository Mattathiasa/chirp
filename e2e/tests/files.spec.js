const { test, expect } = require('@playwright/test');
const { ensureSetUp, openWritablePeer } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('a file can be attached, and shows as a card with a size', async ({ page }) => {
  await openWritablePeer(page);

  const name = `note-${Date.now()}.txt`;
  await page.locator('.composer input[type=file]').setInputFiles({
    name,
    mimeType: 'text/plain',
    buffer: Buffer.from('the quick brown fox jumps over the lazy dog\n'.repeat(200)),
  });

  const card = page.locator('.filecard', { hasText: name });
  await expect(card).toBeVisible();
  // The card states a size rather than leaving the transfer unexplained.
  await expect(card.locator('.n')).toContainText(/\d+(\.\d+)?\s(B|KB|MB)/);
});

test('the attach button is disabled in a room, where transfers do not apply', async ({ page }) => {
  // Make sure there is a room to open, whatever order the specs ran in.
  const peers = await page.request.get('/api/peers').then((r) => r.json());
  const member = peers.find((p) => p.trust !== 'unknown' && p.trust !== 'changed');
  test.skip(!member, 'no pinned peer to build a room with');
  await page.request.post('/api/rooms', {
    headers: { 'X-Chirp': '1' },
    data: { name: `Files ${Date.now()}`, members: [member.name] },
  });
  await page.reload();

  await page.getByRole('button', { name: 'Rooms', exact: true }).first().click();
  await page.locator('.ob').first().click();

  const attach = page.getByRole('button', { name: 'Attach a file' });
  await expect(attach).toBeDisabled();
  await expect(attach).toHaveAttribute('title', /one-to-one/i);
});

// The brief is explicit that a received file must never open itself. The
// daemon serves it as an attachment and the UI offers a download link, so
// nothing the sender names a file can be rendered by the browser.
test('a received file is offered as a download, never rendered', async ({ page }) => {
  await ensureSetUp(page);

  // A demo bot sends one shortly after it connects; wait for it to land and
  // be hash-verified rather than racing it.
  let inbound = null;
  await expect.poll(async () => {
    const files = await page.request.get('/api/files').then((r) => r.json());
    inbound = files.find((f) => f.dir === 'in' && f.status === 'complete');
    return !!inbound;
  }, { timeout: 30_000, message: 'waiting for an inbound transfer to complete' }).toBe(true);

  await page.locator('.row', { hasText: inbound.peer }).first().click();
  const link = page.locator('.filecard a', { hasText: '' }).first();
  await expect(link).toHaveAttribute('download', inbound.name);
  await expect(link).toHaveAttribute('href', new RegExp(`/api/files/${inbound.id}/data$`));

  // And the daemon marks it as an attachment rather than something to render.
  const head = await page.request.get(`/api/files/${inbound.id}/data`);
  expect(head.headers()['content-type']).toBe('application/octet-stream');
  expect(head.headers()['content-disposition']).toContain('attachment');
});
