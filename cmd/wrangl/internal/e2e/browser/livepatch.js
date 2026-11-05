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
    name: 'sheets without JS: Cancel and Create close via invoker commands', url: '/projects/lp',
    async run(page) {
      const dlg = page.locator('dialog#sheet-new');
      await page.locator('button:has-text("New task")').first().click();
      check(await dlg.evaluate((e) => e.open), 'sheet did not open via command=show-modal');
      await dlg.locator('button:has-text("Cancel")').click();
      check(!(await dlg.evaluate((e) => e.open)), 'Cancel (command=close) did not close the sheet');
      await page.locator('button:has-text("New task")').first().click();
      await dlg.locator('input[type=text], input:not([type]), textarea').first().fill('made by the sheet');
      await dlg.locator('button:has-text("Create")').click();
      check(!(await dlg.evaluate((e) => e.open)), 'Create did not close the sheet');
      await page.waitForSelector('li.task:has-text("made by the sheet")', { timeout: 8000 }).catch(() => check(false, 'Create did not post the task (signals not read before close?)'));
    },
  },
  {
    name: 'task page: #split opens the split sheet', url: '/projects/lp',
    async run(page) {
      const href = await page.locator('a.title').first().getAttribute('href');
      await page.goto(base + href + '#split');
      await page.waitForSelector('dialog#sheet-split', { state: 'attached' });
      await page.waitForFunction(() => document.getElementById('sheet-split').open, null, { timeout: 5000 }).catch(() => check(false, '#split did not open the sheet'));
      await patch(page);
      check(await page.locator('dialog#sheet-split').evaluate((e) => e.open && e.matches(':modal')), 'split sheet closed by a live patch');
    },
  },
  {
    // /setup is not a live page, so there is no patch to send; this checks the signal drives the row.
    name: 'setup: account row opens from a per-account signal', url: '/setup',
    async run(page) {
      const body = page.locator('tbody#acct-acme');
      const open = () => body.evaluate((e) => e.classList.contains('open'));
      check(!(await open()), 'account row should start collapsed');
      await body.locator('tr.acct td').first().click();
      check(await open(), 'clicking the row did not expand it');
      await body.locator('.more-toggle').click();
      check(!(await open()), 'the ... button did not collapse it');
      await body.locator('.more-toggle').click();
      check(await open(), 'the ... button did not expand it');
    },
  },
  {
    name: 'backlog: project switcher is a native popover', url: '/projects/lp', width: 390,
    async run(page) {
      const menu = page.locator('#pm-switch');
      const isOpen = () => menu.evaluate((e) => e.matches(':popover-open'));
      check(!(await isOpen()), 'menu should start closed');
      await page.locator('button[popovertarget=pm-switch]').click();
      check(await isOpen(), 'popover did not open');
      await patch(page);
      check(await isOpen(), 'popover closed by a live patch');
      await page.keyboard.press('Escape');
      check(!(await isOpen()), 'Escape did not close the popover');
      await page.locator('button[popovertarget=pm-switch]').click();
      await page.locator('h1').first().click({ force: true });
      check(!(await isOpen()), 'clicking outside did not close the popover');
    },
  },
  {
    name: 'template editor: image choice and arch toggle are signals', url: '/settings/templates/tpl',
    async run(page) {
      const box = page.locator('#tp-ami-box');
      check(!(await box.isVisible()), 'AMI box should start hidden');
      await page.locator('input[type=radio][value=ami]').check();
      check(await box.isVisible(), 'choosing "An AMI id" did not show the AMI field');
      await page.locator('input[type=radio][value=al]').check();
      check(!(await box.isVisible()), 'choosing Amazon Linux did not hide the AMI field');
      const x86 = page.locator('#tp-arch button[role=radio]').nth(1);
      await x86.click();
      check((await x86.getAttribute('aria-checked')) === 'true', 'arch toggle did not check x86_64');
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
    const ctx = await browser.newContext({ viewport: { width: c.width || 1280, height: 900 } });
    const page = await ctx.newPage();
    page.setDefaultTimeout(8000);
    page.on('console', (m) => { if (m.type() === 'error' || process.env.LP_DEBUG) console.error('[console]', c.name, m.text().slice(0, 600)); });
    page.on('pageerror', (e) => console.error('[pageerror]', c.name, String(e).slice(0, 600)));
    if (process.env.LP_DEBUG) console.error('case', c.name);
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
