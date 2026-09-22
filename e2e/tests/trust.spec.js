const { test, expect } = require('@playwright/test');
const { ensureSetUp, waitForPeers } = require('./helpers');

test.beforeEach(async ({ page }) => { await ensureSetUp(page); });

test('a newly pinned peer is called out as trusted on sight, not verified', async ({ page }) => {
  await waitForPeers(page);

  // Peers start as 'unknown' while the handshake runs; wait for one to be
  // pinned rather than racing it.
  let fresh = null;
  await expect.poll(async () => {
    const peers = await page.request.get('/api/peers').then((r) => r.json());
    fresh = peers.find((p) => p.trust === 'new');
    return !!fresh;
  }, { timeout: 20_000, message: 'waiting for a first-contact peer' }).toBe(true);
  await page.reload();

  await page.locator('.row', { hasText: fresh.name }).first().click();
  const banner = page.locator('.banner', { hasText: /first contact/i });
  await expect(banner).toBeVisible();
  await expect(banner).toContainText(/compare fingerprints/i);

  // The banner leads somewhere: the trust panel, with both ways to check.
  await banner.getByRole('button', { name: 'Verify' }).click();
  const panel = page.locator('.panel');
  await expect(panel).toBeVisible();
  await expect(panel).toContainText('Fingerprint');
  await expect(panel).toContainText('Verify with words');
  await expect(panel.locator('.fp span')).toHaveCount(16);
  await expect(panel.locator('.words span')).toHaveCount(6);
});

test('the fingerprint and words shown are the ones the daemon reports', async ({ page }) => {
  await waitForPeers(page);
  const peers = await page.request.get('/api/peers').then((r) => r.json());
  const p = peers.find((x) => x.trust !== 'unknown' && x.trust !== 'changed');
  test.skip(!p, 'no pinned peer');

  await page.locator('.row', { hasText: p.name }).first().click();
  await page.getByRole('button', { name: 'Trust and fingerprint' }).click();
  const panel = page.locator('.panel');
  for (const group of p.fpGroups) await expect(panel.locator('.fp')).toContainText(group);
  for (const w of p.words) await expect(panel.locator('.words')).toContainText(w);
});

test('marking a peer verified sticks, and can be undone', async ({ page }) => {
  await waitForPeers(page);
  const peers = await page.request.get('/api/peers').then((r) => r.json());
  const p = peers.find((x) => x.trust === 'new');
  test.skip(!p, 'no unverified peer to verify');

  await page.locator('.row', { hasText: p.name }).first().click();
  await page.getByRole('button', { name: 'Trust and fingerprint' }).click();
  await page.getByRole('button', { name: /mark verified/i }).click();

  await expect.poll(async () => {
    const now = await page.request.get('/api/peers').then((r) => r.json());
    return now.find((x) => x.name === p.name)?.trust;
  }, { message: 'waiting for the pin to become verified' }).toBe('verified');

  await page.getByRole('button', { name: /remove verified mark/i }).click();
  await expect.poll(async () => {
    const now = await page.request.get('/api/peers').then((r) => r.json());
    return now.find((x) => x.name === p.name)?.trust;
  }).toBe('new');
});

// The demo's impostor bot reappears under the same name with a new key about
// 45 seconds in, which is exactly what a reinstall or an attacker looks like.
test('a changed key blocks sending and shows both fingerprints', async ({ page }) => {
  test.setTimeout(120_000);

  let changed = null;
  await expect.poll(async () => {
    const peers = await page.request.get('/api/peers').then((r) => r.json());
    changed = peers.find((p) => p.trust === 'changed');
    return !!changed;
  }, { timeout: 90_000, intervals: [2000], message: 'waiting for the impostor bot to reappear' }).toBe(true);

  await page.reload();
  await page.locator('.row', { hasText: changed.name }).first().click();

  // Sending is refused until it has been looked at.
  await expect(page.getByRole('textbox', { name: 'Message' })).toBeDisabled();
  const alert = page.locator('.banner', { hasText: /key changed/i });
  await expect(alert).toBeVisible();

  await alert.getByRole('button', { name: 'Review' }).click();
  const panel = page.locator('.panel');
  await expect(panel).toContainText(/new key claiming this name/i);
  await expect(panel).toContainText(/key you pinned before/i);

  // Both fingerprints are on screen, and they differ.
  const groups = panel.locator('.fp');
  await expect(groups).toHaveCount(2);
  const [newFp, oldFp] = await groups.allInnerTexts();
  expect(newFp).not.toBe(oldFp);

  // And the daemon agrees the pin has not moved on its own.
  const fresh = await page.request.get('/api/peers').then((r) => r.json());
  expect(fresh.find((p) => p.name === changed.name).trust).toBe('changed');
});
