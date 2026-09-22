// Onboarding can only happen once per daemon, so each test that exercises it
// gets its own. The demo's -onboard flag starts it with no identity, which is
// otherwise unreachable because the demo normally sets itself up.
const { test: base } = require('@playwright/test');
const { spawn } = require('node:child_process');
const net = require('node:net');
const path = require('node:path');

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

async function waitForReady(url, deadlineMs = 20_000) {
  const until = Date.now() + deadlineMs;
  while (Date.now() < until) {
    try {
      const r = await fetch(url);
      if (r.ok) return;
    } catch { /* not listening yet */ }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`demo did not come up at ${url}`);
}

exports.test = base.extend({
  // A daemon of this test's own, with no identity yet.
  freshApp: async ({}, use) => {
    const port = await freePort();
    const bin = path.resolve(__dirname, '../../bin/chirp-demo');
    const proc = spawn(bin, ['-http', `127.0.0.1:${port}`, '-onboard'], { stdio: 'ignore' });
    const base = `http://127.0.0.1:${port}`;
    try {
      await waitForReady(`${base}/api/state`);
      await use(base);
    } finally {
      proc.kill('SIGTERM');
    }
  },
});

exports.expect = base.expect;
