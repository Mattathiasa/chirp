// Captures the screenshots the README uses, from the real UI running against
// cmd/demo. Not part of the test suite: run it with `make screenshots`.
//
// Each shot waits for the state it is meant to show rather than for a timer,
// so a screenshot can never quietly depict something that had not happened.
const { test, expect } = require('@playwright/test');
const path = require('node:path');
const { ensureSetUp, waitForPeers, openWritablePeer, composer, send } = require('./tests/helpers');

const OUT = path.resolve(__dirname, '../docs/img');
const shot = (page, name) => page.screenshot({ path: path.join(OUT, name), animations: 'disabled' });

test.use({ viewport: { width: 1440, height: 900 } });

test('landing', async ({ page }) => {
  // The landing page only exists before there is an identity.
  const { spawn } = require('node:child_process');
  const net = require('node:net');
  const port = await new Promise((res) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const { port } = s.address(); s.close(() => res(port)); });
  });
  const proc = spawn(path.resolve(__dirname, '../bin/chirp-demo'),
    ['-http', `127.0.0.1:${port}`, '-onboard'], { stdio: 'ignore' });
  try {
    await expect.poll(async () => {
      try { return (await fetch(`http://127.0.0.1:${port}/api/state`)).ok; } catch { return false; }
    }, { timeout: 20_000 }).toBe(true);
    await page.goto(`http://127.0.0.1:${port}`);
    await expect(page.locator('.hero h1')).toBeVisible();
    await shot(page, 'landing.png');

    await page.getByRole('button', { name: 'Get started' }).first().click();
    await page.locator('#nm').fill('Alex');
    await page.getByRole('button', { name: 'Make my key' }).click();
    await expect(page.getByRole('button', { name: 'Continue', exact: true })).toBeEnabled({ timeout: 15_000 });
    await shot(page, 'setup.png');
  } finally { proc.kill('SIGTERM'); }
});

test('chat with the trust panel open', async ({ page }) => {
  await ensureSetUp(page);
  const peer = await openWritablePeer(page);
  await send(page, 'Are we still on for 3?');
  await expect.poll(async () => (await page.locator('.m.them').count()) > 0,
    { timeout: 20_000, message: 'waiting for the bot to answer' }).toBe(true);
  await page.getByRole('button', { name: 'Trust and fingerprint' }).click();
  await expect(page.locator('.panel')).toContainText('Fingerprint');
  await expect(page.locator('.head h2')).toContainText(peer);
  await shot(page, 'chat.png');
});

test('a queued message to an offline peer', async ({ page }) => {
  await ensureSetUp(page);
  await waitForPeers(page);
  let offline = null;
  await expect.poll(async () => {
    const peers = await page.request.get('/api/peers').then((r) => r.json());
    offline = peers.find((p) => !p.online && p.trust !== 'unknown' && p.trust !== 'changed');
    return !!offline;
  }, { timeout: 90_000, intervals: [2000], message: 'waiting for a bot to go offline' }).toBe(true);

  await page.locator('.row', { hasText: offline.name }).first().click();
  await send(page, 'Sending this while you are away.');
  await expect(page.locator('.stat.q').first()).toContainText(/queued/i);
  await shot(page, 'queued.png');
});

test('a changed key', async ({ page }) => {
  test.setTimeout(150_000);
  await ensureSetUp(page);
  let changed = null;
  await expect.poll(async () => {
    const peers = await page.request.get('/api/peers').then((r) => r.json());
    changed = peers.find((p) => p.trust === 'changed');
    return !!changed;
  }, { timeout: 120_000, intervals: [2000], message: 'waiting for the impostor bot' }).toBe(true);

  await page.reload();
  await page.locator('.row', { hasText: changed.name }).first().click();
  await page.locator('.banner', { hasText: /key changed/i }).getByRole('button', { name: 'Review' }).click();
  await expect(page.locator('.panel')).toContainText(/new key claiming this name/i);
  await shot(page, 'changed.png');
});

test('a file transfer', async ({ page }) => {
  await ensureSetUp(page);
  let inbound = null;
  await expect.poll(async () => {
    const files = await page.request.get('/api/files').then((r) => r.json());
    inbound = files.find((f) => f.dir === 'in' && f.status === 'complete');
    return !!inbound;
  }, { timeout: 40_000, message: 'waiting for the demo attachment' }).toBe(true);

  await page.locator('.row', { hasText: inbound.peer }).first().click();
  await expect(page.locator('.filecard', { hasText: inbound.name })).toBeVisible();
  await shot(page, 'files.png');
});

test('rooms', async ({ page }) => {
  await ensureSetUp(page);
  await waitForPeers(page);

  // Build a room to photograph. Anything less than a real one on screen is
  // not worth shipping in the README, so this fails rather than settles.
  const peers = await page.request.get('/api/peers').then((r) => r.json());
  const members = peers.filter((p) => p.trust !== 'unknown' && p.trust !== 'changed').slice(0, 2);
  expect(members.length, 'need at least one pinned peer to build a room').toBeGreaterThan(0);
  const created = await page.request.post('/api/rooms', {
    headers: { 'X-Chirp': '1' },
    data: { name: 'Thursday standup', members: members.map((m) => m.name) },
  });
  expect(created.status()).toBe(201);

  await page.reload();
  await page.getByRole('button', { name: 'Rooms', exact: true }).first().click();
  await expect(page.locator('.main')).toContainText('Rooms');

  await page.locator('.ob', { hasText: 'Thursday standup' }).first().click();
  await expect(page.locator('.head h2')).toContainText('Thursday standup');

  await send(page, 'Anyone got the HDMI adapter?');
  await page.getByRole('button', { name: 'Members' }).click();
  await expect(page.locator('.panel')).toContainText('Members');
  await shot(page, 'rooms.png');
});

test('nearby with the invite QR', async ({ page }) => {
  await ensureSetUp(page);
  await page.getByRole('button', { name: 'Nearby', exact: true }).first().click();
  await expect(page.locator('.main')).toContainText('Nearby');
  await expect(page.locator('.qr svg')).toBeVisible();
  await expect(page.locator('.fp')).toContainText('chirp://add/');
  await shot(page, 'nearby.png');
});

test('network and settings', async ({ page }) => {
  await ensureSetUp(page);
  await page.getByRole('button', { name: 'Network', exact: true }).first().click();
  await expect(page.locator('.main')).toContainText('Network and outbox');
  await shot(page, 'network.png');

  const back = page.locator('.back:visible').first();
  if (await back.isVisible().catch(() => false)) await back.click();
  await page.getByRole('button', { name: 'Settings', exact: true }).first().click();
  await expect(page.locator('.main')).toContainText('Keep messages for');
  await shot(page, 'settings.png');
});
