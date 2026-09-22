const { test, expect } = require('./fresh');

// A daemon with no identity shows the landing page, not the app.
test('setup walks landing, name, real key generation, backup and ready', async ({ page, freshApp }) => {
  await page.goto(freshApp);
  const landing = page.locator('.landing');

  // Landing explains itself before asking for anything.
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/talk to the room/i);
  await expect(landing).toContainText(/no server/i);
  await expect(page.locator('.faq details')).not.toHaveCount(0);

  await page.getByRole('button', { name: 'Get started' }).first().click();

  // Step 1: a name is required before the key can be made.
  const go = page.getByRole('button', { name: 'Make my key' });
  await expect(go).toBeDisabled();
  await page.locator('#nm').fill('Tester');
  await expect(go).toBeEnabled();
  await go.click();

  // Step 2: the terminal prints the real fingerprint the daemon returned.
  const term = page.locator('.term');
  await expect(term).toBeVisible();
  const cont = page.getByRole('button', { name: 'Continue', exact: true });
  await expect(cont).toBeEnabled({ timeout: 15_000 });

  const me = await page.request.get(`${freshApp}/api/me`).then((r) => r.json());
  expect(me.fp).toMatch(/^[0-9a-f]{64}$/);
  await expect(term).toContainText(me.fp.slice(0, 32));
  await expect(term).toContainText(me.words.join(' '));
  await cont.click();

  // Step 3: the export button stays locked until the passphrase is long enough.
  const save = page.getByRole('button', { name: /export encrypted backup/i });
  await expect(save).toBeDisabled();
  await page.getByLabel('Backup passphrase').fill('short');
  await expect(save).toBeDisabled();
  await page.getByLabel('Backup passphrase').fill('a-long-enough-passphrase-9');
  await expect(save).toBeEnabled();

  await page.getByRole('button', { name: /skip for now/i }).click();

  // Step 4: into the app.
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/set/i);
  await page.getByRole('button', { name: 'Open Chirp' }).click();
  await expect(page.locator('.stage')).toBeVisible();
});

test('the strength meter agrees with what the daemon will accept', async ({ page, freshApp }) => {
  await page.goto(freshApp);
  await page.getByRole('button', { name: 'Get started' }).first().click();
  await page.locator('#nm').fill('Tester');
  await page.getByRole('button', { name: 'Make my key' }).click();
  await expect(page.getByRole('button', { name: 'Continue', exact: true })).toBeEnabled({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Continue', exact: true }).click();

  const field = page.getByLabel('Backup passphrase');
  const meter = page.locator('.bars');

  // Eleven characters is under the daemon's minimum, so export stays locked.
  await field.fill('elevenchars');
  await expect(page.getByRole('button', { name: /export encrypted backup/i })).toBeDisabled();
  await expect(meter).toHaveAttribute('aria-label', /Too short|Weak/);

  await field.fill('Tr0ub4dor&3-horse-battery');
  await expect(meter).toHaveAttribute('aria-label', /Strong|Excellent/);
});

test('restore is reachable from the landing page and rejects a bad file', async ({ page, freshApp }) => {
  await page.goto(freshApp);

  await page.getByRole('button', { name: 'Restore a backup' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/bring your key/i);

  const restore = page.getByRole('button', { name: 'Restore my key' });
  await expect(restore).toBeDisabled();

  await page.getByLabel('Backup file').setInputFiles({
    name: 'not-a-backup.json', mimeType: 'application/json', buffer: Buffer.from('{"nope":1}'),
  });
  await page.getByLabel('Backup passphrase').fill('whatever-passphrase');
  await expect(restore).toBeEnabled();
  await restore.click();

  await expect(page.locator('.err')).not.toBeEmpty();
  await expect(page.locator('.stage')).toHaveCount(0);
});
