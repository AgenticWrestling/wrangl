// Multi-repo project surfaces (ADR 0012 section 7) at desktop and phone width: the "needs repo" warning
// chip and backlog filter, the repo chip and picker on the task page, the queue sheet, and the project's
// default-repo selector; a single-repo project shows none of it.
// usage: node repos.js <playwright dir> <base url> <repo-less task> <task with repo> <single-repo project> <second repo-less task>
const [pwDir, base, needSqid, chipSqid, single, need2Sqid] = process.argv.slice(2);
const { chromium } = require(pwDir + '/node_modules/playwright');
const errors = [];
const check = (ok, msg) => { if (!ok) errors.push(msg); };
const noOverflow = async (page, name, what) => {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  check(over <= 1, `${name}: ${what} overflows by ${over}px`);
};

(async () => {
  const browser = await chromium.launch();
  for (const [name, width, height] of [['desktop', 1280, 800], ['phone', 390, 844]]) {
    const ns = name === 'desktop' ? needSqid : need2Sqid; // the desktop pass queues its task, so each pass has its own
    const page = await browser.newPage({ viewport: { width, height } });

    // Backlog: a warning chip on the task that resolves to no repo, and the filter.
    await page.goto(base + '/projects/multi');
    const row = page.locator(`li.task[data-sqid="${ns}"]`);
    await row.waitFor({ timeout: 8000 });
    check((await row.first().innerText()).includes('needs repo'), `${name}: no warning chip on the repo-less task`);
    check(!(await page.locator(`li.task[data-sqid="${chipSqid}"]`).first().innerText()).includes('needs repo'), `${name}: warning chip on a task that has a repo`);
    check((await page.locator(`li.task[data-sqid="${chipSqid}"] .tag.repo`).first().innerText()).includes('o/b'), `${name}: no repo chip on the task that names one`);
    const filter = page.locator('#backlog a.chip', { hasText: 'needs repo' });
    check(await filter.count() === 1, `${name}: no needs-repo filter`);
    await noOverflow(page, name, 'multi-repo backlog');
    await page.goto(base + '/projects/multi?needsrepo=1');
    await page.locator(`li.task[data-sqid="${ns}"]`).first().waitFor({ timeout: 8000 }).catch(async () => errors.push(name + ': filter page: ' + (await page.locator('#backlog').innerHTML()).slice(0, 500)));
    check(await page.locator(`li.task[data-sqid="${chipSqid}"]`).count() === 0, `${name}: the filter still lists a task that has a repo`);

    // Task page: warning chip, repo picker in Edit, queue sheet.
    await page.goto(base + '/task/' + ns);
    await page.locator('.meta .tag.wait').waitFor({ timeout: 8000 }).catch(async () => { errors.push(name + ': task page meta: ' + (await page.locator('.meta').first().innerHTML()).slice(0, 600)); });
    await page.locator('button[command=show-modal][commandfor=sheet-edit]').click();
    check(await page.locator('#sheet-edit select[data-bind=erepo]').isVisible(), `${name}: no repo picker in the edit sheet`);
    await noOverflow(page, name, 'edit sheet');
    await page.keyboard.press('Escape');
    await page.goto(base + '/task/' + chipSqid);
    check((await page.locator('.meta .tag.repo').first().innerText()).includes('o/b'), `${name}: task page lacks the repo chip`);
    const qTask = ns;
    await page.goto(base + '/task/' + qTask);
    await page.locator('button[command=show-modal][commandfor=sheet-queue]').click();
    const qsel = page.locator('#sheet-queue select');
    check(await qsel.isVisible(), `${name}: queue sheet has no repo select`);
    check((await qsel.inputValue()) === '', `${name}: queue sheet preselects a repo for a task that has none`);
    await noOverflow(page, name, 'queue sheet');
    if (name === 'desktop') { // choose a repo and queue: the task gets it and the run is queued
      await qsel.selectOption('o/a');
      await page.locator('#sheet-queue button.primary').click();
      await page.waitForTimeout(800);
    } else {
      await page.keyboard.press('Escape');
    }

    // Project page: default-repo selector and notes.
    await page.goto(base + '/projects/multi/settings');
    const sel = page.locator('#default-repo');
    await sel.waitFor({ timeout: 8000 });
    check((await page.locator('#repos').innerText()).includes('API server'), `${name}: repo note not shown`);
    await noOverflow(page, name, 'project settings');
    if (name === 'desktop') {
      await sel.selectOption('o/a');
      await page.waitForTimeout(800);
      await page.goto(base + '/projects/multi/settings');
      check((await page.locator('#default-repo').inputValue()) === 'o/a', `${name}: default repo did not stick`);
      await page.locator('#default-repo').selectOption(''); // back to "ask me" for the phone pass
      await page.waitForTimeout(800);
      await page.goto(base + '/projects/multi/settings');
      check((await page.locator('#default-repo').inputValue()) === '', `${name}: default repo did not clear`);
    }

    // A single-repo project shows none of it.
    await page.goto(base + '/projects/' + single + '/settings');
    await page.locator('#repos').waitFor({ timeout: 8000 });
    check(await page.locator('#default-repo').count() === 0, `${name}: single-repo project shows the default-repo selector`);
    await page.goto(base + '/projects/' + single);
    await page.locator('#backlog').waitFor({ timeout: 8000 });
    check((await page.locator('#backlog').innerText()).indexOf('needs repo') < 0 && await page.locator('#backlog .tag.repo').count() === 0, `${name}: single-repo backlog shows repo chips`);
    await page.screenshot({ path: process.env.SHOT_DIR ? process.env.SHOT_DIR + '/repos-' + name + '.png' : '/dev/null' }).catch(() => {});
    await page.close();
  }
  await browser.close();
  console.log(JSON.stringify({ errors }));
})().catch((e) => { console.log(JSON.stringify({ errors: [String(e)] })); process.exit(0); });
