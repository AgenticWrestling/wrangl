// Live-patch survival checks. For each case: load a page, put the UI in a client-only state, inject a
// probe node into #main, make the server patch #main (hook = create a task), wait for the probe to
// vanish (proof a patch landed), then assert the state is still there.
// usage: node livepatch.js <playwright dir> <base url> <hook url>   (prints {"errors":[...]})
const [pwDir, base, hook] = process.argv.slice(2);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };

async function patch(page) {
  await page.evaluate(() => { const p = document.createElement('i'); p.id = 'probe'; document.getElementById('main').appendChild(p); });
  await page.evaluate((h) => fetch(h, { mode: 'no-cors' }), hook);
  await page.waitForFunction(() => !document.getElementById('probe'), null, { timeout: 8000 });
  await page.waitForTimeout(150);
}

const cases = [
  {
    name: 'settings: agent form <details>', url: '/settings',
    async run(page) {
      const d = page.locator('details:has(> summary:text("Add or edit an agent"))');
      await d.locator('> summary').click();
      check(await d.evaluate((e) => e.open), 'agent details did not open');
      await patch(page);
      check(await d.evaluate((e) => e.open), 'agent details closed by a live patch');
    },
  },
  {
    name: 'backlog: New task sheet', url: '/projects/lp',
    async run(page) {
      await page.locator('button:has-text("New task")').first().click();
      const dlg = page.locator('dialog#sheet-new');
      check(await dlg.evaluate((e) => e.open && e.matches(':modal')), 'New task sheet did not open modally');
      await dlg.locator('input[type=text], input:not([type]), textarea').first().fill('typed text');
      await patch(page);
      check(await dlg.evaluate((e) => e.open && e.matches(':modal')), 'New task sheet closed by a live patch');
      check((await dlg.locator('input[type=text], input:not([type]), textarea').first().inputValue()) === 'typed text', 'typed text lost');
    },
  },
  {
    name: 'project page: repo sheet', url: '/projects/lp/settings',
    async run(page) {
      await page.locator('button:has-text("Link repo")').click();
      const dlg = page.locator('dialog#sheet-repo');
      check(await dlg.evaluate((e) => e.open), 'repo sheet did not open');
      await patch(page);
      check(await dlg.evaluate((e) => e.open && e.matches(':modal')), 'repo sheet closed by a live patch');
    },
  },
];

(async () => {
  const browser = await chromium.launch();
  for (const c of cases) {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await ctx.newPage();
    try {
      await page.goto(base + c.url);
      await page.waitForSelector('#main', { state: 'attached' });
      await page.waitForTimeout(500); // let the live stream connect
      await c.run(page);
    } catch (e) {
      errors.push(`[${c.name}] exception: ` + e.message.replace(/\s+/g, ' ').slice(0, 400));
    }
    await ctx.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})();
