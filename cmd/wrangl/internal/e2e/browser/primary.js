// Primary session button and terminal tab at desktop and phone width.
// usage: node primary.js <playwright dir> <base url>   (prints {"errors":[...]})
const [pwDir, base] = process.argv.slice(2);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };

(async () => {
  const browser = await chromium.launch();
  for (const [name, vp] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
    const page = await browser.newPage({ viewport: vp });
    // Projects page: a pill on alpha's row, none on gamma's; the Session button is in the expanded row.
    await page.goto(base + '/projects');
    const alpha = page.locator('#proj-alpha');
    await alpha.waitFor({ timeout: 8000 });
    check((await alpha.innerText()).includes('session running'), name + ': projects page lacks the running pill: ' + (await alpha.innerText()));
    check(!(await page.locator('#proj-gamma').innerText()).includes('session'), name + ': gamma (no repo) shows a session pill');
    await alpha.locator('tr').first().click();
    const pbtn = alpha.locator('a.sessbtn');
    check(await pbtn.count() === 1 && await pbtn.isVisible(), name + ': Session button missing in the expanded project row');
    check(await page.locator('#proj-gamma a.sessbtn').count() === 0, name + ': gamma has a Session button');
    let over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    check(over <= 1, name + ': projects page overflows by ' + over + 'px');

    // Project page: pill, health text and button.
    await page.goto(base + '/projects/alpha');
    const strip = page.locator('.primary-session');
    await strip.waitFor({ timeout: 8000 });
    const txt = await strip.innerText();
    check(txt.includes('session running') && txt.includes('running, pid'), name + ': project page health: ' + txt);
    check(await strip.locator('a.sessbtn').count() === 1, name + ': project page Session button missing');
    over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    check(over <= 1, name + ': project page overflows by ' + over + 'px');
    await page.goto(base + '/projects/gamma');
    check(await page.locator('.primary-session a.sessbtn').count() === 0, name + ': gamma page has a Session button');

    // The button opens the host terminal on a tab named after the project.
    await page.goto(base + '/projects/alpha');
    await page.locator('.primary-session a.sessbtn').click();
    await page.waitForURL(/\/runner\/host-test\/term#primary-alpha/, { timeout: 8000 });
    const tab = page.locator('#ttabs [role=tab]', { hasText: 'alpha' });
    await tab.waitFor({ timeout: 8000 });
    check((await tab.getAttribute('aria-selected')) === 'true', name + ': the alpha tab is not selected');
    await page.waitForFunction(() => /^live/.test(document.getElementById('tstate').textContent), null, { timeout: 8000 });

    // Watch-only until Take control: typing before it does nothing; after it, cat echoes.
    const marker = 'watchonly' + name;
    await page.locator('#tstage .tpane.on').click({ position: { x: 20, y: 20 } }).catch(() => {});
    await page.keyboard.type(marker + '\n');
    await page.waitForTimeout(600);
    let screen = await page.locator('#tstage .tpane.on').innerText();
    check(!screen.includes(marker), name + ': typing reached the session before Take control');
    await page.locator('#tctl').click();
    await page.waitForFunction(() => /typing/.test(document.getElementById('tstate').textContent), null, { timeout: 8000 });
    await page.waitForTimeout(800);
    const typed = 'typed' + name;
    await page.keyboard.type(typed + '\n');
    await page.waitForFunction((m) => document.querySelector('#tstage .tpane.on').innerText.includes(m), typed, { timeout: 8000 })
      .catch(() => check(false, name + ': typed text did not come back after Take control'));
    await page.screenshot({ path: process.env.SHOT_DIR ? process.env.SHOT_DIR + '/primary-' + name + '.png' : '/dev/null' }).catch(() => {});
    await page.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})().catch((e) => { console.log(JSON.stringify({ errors: ['script: ' + e.message] })); });
