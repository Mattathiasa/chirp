const { test, expect } = require('@playwright/test');

// A fresh daemon has no identity, so the first thing anyone sees is this.
test('a new install asks for a name and creates a key', async ({ page }) => {
  await page.goto('/');
  const nameField = page.locator('#nm');

  // Either we are at onboarding, or a previous test already set the demo up.
  if (!(await nameField.isVisible().catch(() => false))) {
    test.skip(true, 'this demo instance already has an identity');
  }

  const go = page.getByRole('button', { name: /create my key/i });
  await expect(go).toBeDisabled();

  await nameField.fill('Tester');
  await expect(go).toBeEnabled();
  await go.click();

  await expect(page.locator('.stage')).toBeVisible();
  // The identity is real: a fingerprint exists for it.
  const state = await page.request.get('/api/me').then((r) => r.json());
  expect(state.fp).toMatch(/^[0-9a-f]{64}$/);
  expect(state.words).toHaveLength(6);
});
