// Workers page checks at desktop and phone width: nav label, Sessions list with the managed /
// unmanaged count and a terminal link, no horizontal overflow, and /runners redirecting.
// usage: node workers.js <playwright dir> <base url>   (prints {"errors":[...]})
const [pwDir, base] = process.argv.slice(2);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };

(async () => {
  const browser = await chromium.launch();
  for (const [name, vp] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
    const page = await browser.newPage({ viewport: vp });
    await page.goto(base + '/runners');
    check(new URL(page.url()).pathname === '/workers', name + ': /runners did not redirect to /workers: ' + page.url());
    const card = page.locator('.card.runner', { hasText: 'w-test' });
    await card.waitFor({ timeout: 8000 });
    const nav = await page.locator('a[href="/workers"]').allInnerTexts();
    check(nav.some((t) => t.includes('Workers')), name + ': nav label is not Workers: ' + JSON.stringify(nav));
    check((await page.title()).includes('Workers') || (await page.content()).includes('Workers'), name + ': no Workers title');
    const text = await card.innerText();
    check(text.includes('2 managed') && text.includes('1 unmanaged'), name + ': counts missing: ' + text);
    check(text.includes('hand'), name + ': RC name missing');
    check(await card.locator('a[href="/runner/w-test/term#run-7"]').count() === 1, name + ': run terminal link missing');
    const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    check(over <= 1, name + ': horizontal overflow of ' + over + 'px');
    await page.screenshot({ path: process.env.SHOT_DIR ? process.env.SHOT_DIR + '/workers-' + name + '.png' : '/dev/null' }).catch(() => {});
    await page.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})().catch((e) => { console.log(JSON.stringify({ errors: ['script: ' + e.message] })); });
