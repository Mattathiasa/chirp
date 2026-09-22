// Shared helpers. The demo starts with no identity, so almost everything needs
// to get through onboarding first.
const { expect } = require('@playwright/test');

/** Complete onboarding if the welcome screen is showing. */
async function ensureSetUp(page, name = 'Tester') {
  await page.goto('/');
  const nameField = page.locator('#nm');
  if (await nameField.isVisible().catch(() => false)) {
    await nameField.fill(name);
    await page.getByRole('button', { name: /create my key/i }).click();
  }
  await expect(page.locator('.stage')).toBeVisible();
}

/** Wait for at least one scripted bot to be discovered. */
async function waitForPeers(page) {
  await expect(page.locator('.row').first()).toBeVisible({ timeout: 20_000 });
}

/** The message box. */
function composer(page) {
  return page.getByRole('textbox', { name: 'Message' });
}

/**
 * Open a conversation that can actually be typed into: a pinned peer with an
 * open session. A peer still shaking hands has a disabled composer, and one
 * whose key changed is deliberately locked.
 */
async function openWritablePeer(page) {
  await waitForPeers(page);
  let name = null;
  await expect.poll(async () => {
    const peers = await page.request.get('/api/peers').then((r) => r.json());
    const ok = peers.find((p) => p.online && p.trust !== 'unknown' && p.trust !== 'changed');
    name = ok ? ok.name : null;
    return name;
  }, { timeout: 20_000, message: 'waiting for a peer with an open session' }).not.toBeNull();

  await page.locator('.row', { hasText: name }).first().click();
  await expect(composer(page)).toBeEnabled();
  return name;
}

/** Type a message, send it, and wait for it to appear. */
async function send(page, body) {
  const box = composer(page);
  await box.fill(body);
  await box.press('Enter');
  await expect(page.locator('.bubble', { hasText: body }).first()).toBeVisible();
  return body;
}

/**
 * Navigate to a section at any width. On a phone the conversation list and the
 * open view are two levels: the pane holding the navigation is hidden while a
 * view is open, so step back to the list first.
 */
async function goToView(page, label) {
  const btn = page.getByRole('button', { name: label, exact: true }).first();
  const deadline = Date.now() + 20_000;

  // The list updates in the background as peers come and go, so a plain
  // check-then-click races those updates. Retry the whole thing.
  while (Date.now() < deadline) {
    if (await btn.isVisible().catch(() => false)) {
      try {
        await btn.click({ timeout: 2_000 });
        return;
      } catch { /* the view changed under us; take stock and try again */ }
    }
    const back = page.locator('.back:visible').first();
    if (await back.isVisible().catch(() => false)) {
      await back.click({ timeout: 2_000 }).catch(() => {});
    }
    await page.waitForTimeout(200);
  }
  throw new Error(`could not reach the ${label} view`);
}

module.exports = { ensureSetUp, waitForPeers, openWritablePeer, composer, send, goToView };
