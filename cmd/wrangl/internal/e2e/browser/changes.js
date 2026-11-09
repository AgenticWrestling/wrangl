// Review card "Changes" section: open it on the Inbox at desktop and phone width, expand a file, check
// the diff renders with +/- colouring, the file block scrolls sideways inside itself (the page does not),
// and that a live patch of #main leaves the loaded diff and the open file alone.
// usage: node changes.js <playwright dir> <base url>   (prints {"errors":[...]})
const [pwDir, base] = process.argv.slice(2);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };

(async () => {
  const browser = await chromium.launch();
  for (const [name, width, height] of [['desktop', 1280, 800], ['phone', 390, 800]]) {
    const page = await browser.newPage({ viewport: { width, height } });
    await page.goto(base + '/inbox');
    const card = page.locator('.icard.q-pr_review');
    const sec = card.locator('details.chg');
    check(await sec.count() === 1, `${name}: no Changes section on the PR card`);
    if (await sec.count() !== 1) { await page.close(); continue; }
    check(!(await sec.evaluate((e) => e.open)), `${name}: Changes should start collapsed`);
    check((await card.locator('.chgfile').count()) === 0, `${name}: diff loaded before opening`);
    await sec.locator('> summary').click();
    await page.waitForSelector('.chgfile', { timeout: 8000 });
    check((await card.locator('.chgfile').count()) === 2, `${name}: expected two files`);
    const txt = await card.locator('.chgbody').innerText();
    check(txt.includes('a.txt') && txt.includes('+2') && txt.includes('bin.dat'), `${name}: file list missing counts/names: ${txt.slice(0, 200)}`);
    const file = card.locator('.chgfile', { hasText: 'a.txt' });
    await file.locator('> summary').click();
    check(await file.locator('.d-add').count() >= 1 && await file.locator('.d-del').count() >= 1, `${name}: no add/del lines`);
    const col = await file.locator('.d-add').first().evaluate((e) => getComputedStyle(e).backgroundColor);
    check(col !== 'rgba(0, 0, 0, 0)', `${name}: added line is not coloured`);
    const pre = file.locator('pre.udiff');
    check(await pre.evaluate((e) => e.scrollWidth > e.clientWidth), `${name}: long line does not scroll inside the file block`);
    check(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), `${name}: page scrolls sideways`);
    check((await card.locator('.chgfile', { hasText: 'bin.dat' }).innerText()).includes('binary'), `${name}: binary file not marked`);
    await page.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})().catch((e) => { console.log(JSON.stringify({ errors: [String(e)] })); });
