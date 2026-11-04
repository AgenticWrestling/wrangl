// Drives the project chat in a real browser at a desktop and a phone viewport.
// usage: node chat.js <playwright dir> <base url> '<json: {desktop: {project, solo}, phone: {project, solo}}>'   (prints one JSON line: {"errors":[...]})
const [pwDir, base, targetsJson] = process.argv.slice(2);
const targets = JSON.parse(targetsJson);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };

(async () => {
  const browser = await chromium.launch();
  for (const vp of [
    { name: 'desktop', width: 1280, height: 800, phone: false },
    { name: 'phone', width: 390, height: 844, phone: true },
  ]) {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, hasTouch: vp.phone, isMobile: vp.phone });
    const page = await ctx.newPage();
    const { project, solo } = targets[vp.name];
    if (process.env.CHAT_SHOTS) { page.on('console', (m) => console.error('console', m.text())); page.on('requestfailed', (r) => console.error('reqfail', r.url())); page.on('response', (r) => { if (r.status() >= 400) console.error('http', r.status(), r.url()); }); }
    const tag = (m) => `[${vp.name}] ${m}`;
    try {
      await page.goto(base + '/projects/' + project);
      await page.waitForSelector('#projchat', { state: 'attached' });
      const panel = page.locator('#projchat');
      check(!(await panel.isVisible()), tag('chat must start closed'));
      const fab = page.locator('.chatfab');
      check((await fab.isVisible()) === vp.phone, tag('floating button visible only on phones'));
      if (vp.phone) await fab.click(); else await page.getByRole('button', { name: 'Comment', exact: true }).click();
      await panel.waitFor({ state: 'visible', timeout: 5000 });
      const box = await panel.boundingBox();
      if (vp.phone) check(box.width >= vp.width - 1 && box.height >= vp.height - 1, tag(`chat should be full screen, got ${JSON.stringify(box)}`));
      else check(box.x >= vp.width - 410 && box.width <= 410 && box.height >= vp.height - 1, tag(`chat should be a right-hand panel, got ${JSON.stringify(box)}`));
      const chipText = await page.locator('.chatchip').first().textContent();
      check(/3 tasks/.test(chipText), tag('context chip shows the task count, got ' + JSON.stringify(chipText)));
      if (vp.phone) check(!(await fab.isVisible()), tag('floating button hides while the chat is open'));

      // An ambiguous /queue answers with approve buttons (no model involved).
      await page.locator('#projchat textarea').fill('/queue alpha');
      await page.getByRole('button', { name: 'Send' }).click();
      const approve = page.locator('#projchat .props button').first();
      await approve.waitFor({ state: 'attached', timeout: 8000 }).catch(() => { throw new Error('no approve buttons in the reply'); });
      const apText = await approve.textContent();
      check(/Approve/.test(apText), tag('proposal button says Approve, got ' + JSON.stringify(apText)));
      await approve.click();
      await page.waitForSelector('#projchat .props button[disabled]', { timeout: 8000 }).catch(() => { throw new Error('approving did not mark the option done'); }).catch(async (e) => { throw new Error(e.message + ': ' + (await page.locator('#projchat .props').first().textContent()).replace(/\s+/g, ' ') + ' / toast: ' + (await page.locator('#toast').textContent())); });

      // A command on a single task replies with a linked task id; clicking it jumps to the row and flashes it.
      await page.locator('#projchat textarea').fill('/queue ' + solo);
      await page.getByRole('button', { name: 'Send' }).click();
      const link = page.locator(`#projchat a.tlink[data-goto="${solo}"]`).last();
      await link.waitFor({ state: 'attached', timeout: 8000 }).catch(() => { throw new Error('reply has no task link for ' + solo); }).catch(async (e) => { throw new Error(e.message + ': ' + (await page.locator('#projchat .chatlog').innerHTML()).slice(-300) + ' / toast: ' + (await page.locator('#toast').textContent()) + ' / ta: ' + (await page.locator('#projchat textarea').inputValue())); });
      await link.dispatchEvent('click'); // text may have no glyphs in a headless box without system fonts, so do not require a visible size
      await page.waitForSelector(`li.task[data-sqid="${solo}"].flash`, { timeout: 5000 }).catch(() => { throw new Error('row did not flash after following the link'); });
      if (vp.phone) check(!(await panel.isVisible()), tag('following a task link closes the full-screen chat'));
    } catch (e) {
      if (process.env.CHAT_SHOTS) await page.screenshot({ path: process.env.CHAT_SHOTS + '/fail-' + vp.name + '.png' });
      errors.push(tag('exception: ' + e.message.replace(/\s+/g, ' ')));
    }
    await ctx.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})();
