// Optional browser regression checks; dependencies live outside the application.
const { chromium } = require('playwright');
const AxeBuilder = require('@axe-core/playwright').default;
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');

(async () => {
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
    const page = await context.newPage();
    const base = process.argv[2];
    const screenshots = process.env.RECEIPT_UI_SCREENSHOTS || path.join(os.tmpdir(), 'receipt-ui-screenshots');
    fs.mkdirSync(screenshots, { recursive: true });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    async function check(name) {
      // Native PDF viewers are browser-owned; audit the application around them.
      const report = await new AxeBuilder({ page }).exclude('iframe').withTags(['wcag2a', 'wcag2aa', 'wcag21aa']).analyze();
      assert.deepEqual(report.violations.map(v => ({ id: v.id, nodes: v.nodes.map(n => n.target) })), [], name + ': accessibility');
      const overflow = await page.evaluate(() => ({ width: innerWidth, actual: document.documentElement.scrollWidth, elements: [...document.querySelectorAll('main *')].filter(e => e.getBoundingClientRect().right > innerWidth && !e.closest('.table-wrap')).map(e => e.tagName + '.' + e.className) }));
      assert.ok(overflow.actual <= overflow.width, name + ': page overflow ' + JSON.stringify(overflow));
      await page.screenshot({ path: path.join(screenshots, name + '.png'), fullPage: true });
    }
    await page.goto(base + '/receipts');
    await page.getByRole('heading', { name: 'Welcome back' }).waitFor();
    await check('login');
    await page.getByLabel('Password').fill('fixture-password');
    await page.getByRole('button', { name: 'Log in', exact: true }).click();
    await page.getByRole('heading', { name: 'Operations', exact: true }).waitFor();
    await check('operations');
    await page.getByRole('link', { name: 'Receipt queue', exact: true }).click();
    await page.getByText('Page 1 of 2', { exact: true }).waitFor();
    await check('queue-desktop');
    await page.getByRole('button', { name: 'Next →' }).click();
    await page.getByText('Page 2 of 2', { exact: true }).waitFor();
    await page.getByLabel('Find a receipt').fill('receipt-30');
    await page.getByRole('button', { name: 'Apply filters' }).click();
    await page.getByText('1 of 31 receipts · newest imports first', { exact: true }).waitFor();
    await page.getByRole('link', { name: 'receipt-30.pdf', exact: true }).click();
    await page.getByRole('heading', { name: 'receipt-30.pdf', exact: true }).waitFor();
    const detailURL = page.url();
    const original = await page.request.get(base + new URL(detailURL).pathname + '/document');
    assert.equal(original.status(), 200);
    assert.ok((await original.body()).subarray(0, 8).toString().startsWith('%PDF-1.4'));
    const matchLayout = await page.locator('#suggested-matches').evaluate(el => ({
      inWorkspace: !!el.closest('.receipt-detail'),
      tableFits: el.querySelector('.table-wrap').scrollWidth <= el.querySelector('.table-wrap').clientWidth,
      firstSuggestionVisible: el.querySelector('input[type="radio"]').getBoundingClientRect().bottom <= innerHeight,
    }));
    assert.ok(matchLayout.inWorkspace, 'suggested matches should sit beside the original document');
    assert.ok(matchLayout.tableFits, 'desktop suggestions should not require horizontal scrolling');
    assert.ok(matchLayout.firstSuggestionVisible, 'first suggestion should be visible without scrolling on desktop');
    await check('review-desktop');
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole('link', { name: 'Jump to suggested matches ↓' }).click();
    assert.equal(new URL(page.url()).hash, '#suggested-matches');
    const mobileMatchTable = await page.locator('#suggested-matches .table-wrap').evaluate(el => el.scrollWidth <= el.clientWidth);
    assert.ok(mobileMatchTable, 'mobile suggestions should not require horizontal scrolling');
    await check('review-mobile');
    await page.goto(base + '/receipts');
    await check('queue-mobile');
    await page.goto(base + '/xero/transactions');
    await check('transactions-mobile');
    await page.goto(detailURL);
    await page.getByRole('button', { name: 'Correct receipt details ↓' }).click();
    assert.equal(new URL(page.url()).hash, '#correct-receipt');
    await page.getByLabel('Merchant', { exact: true }).fill('Shop corrected');
    await page.getByRole('button', { name: 'Save correction' }).click();
    await page.getByRole('heading', { name: 'Shop corrected', exact: true }).waitFor();
    const queueContext = new URL(detailURL).searchParams.get('queue');
    assert.equal(new URL(page.url()).searchParams.get('queue'), queueContext, 'correction lost queue filters');
    await page.getByLabel('Contact, reference, account, date, amount or ID', { exact: true }).fill('fixture-transaction');
    await page.getByRole('button', { name: 'Search', exact: true }).click();
    assert.equal(new URL(page.url()).searchParams.get('queue'), queueContext, 'transaction search lost queue filters');
    const suggestion = page.getByLabel('Select suggestion fixture-transaction', { exact: true });
    assert.equal(await suggestion.isChecked(), false, 'approval must require explicit selection');
    await page.getByRole('button', { name: 'Approve suggested match' }).click();
    assert.equal(await suggestion.evaluate(e => e.validity.valueMissing), true);
    await suggestion.check();
    await page.getByRole('button', { name: 'Approve suggested match' }).click();
    await page.getByText(/Approved for fixture-transaction/).waitFor();
    assert.equal(await page.getByRole('button', { name: 'Approve suggested match' }).count(), 0);
    // Work through a filtered queue without returning to the list each time.
    await page.goto(base + '/receipts?q=Shop&state=needs_review&attention=1');
    await page.locator('.receipt-name').first().click();
    const firstPath = new URL(page.url()).pathname;
    const topNavigation = page.getByRole('navigation', { name: 'Review queue navigation' }).first();
    assert.equal(await topNavigation.getByRole('button', { name: '← Previous receipt' }).isDisabled(), true);
    await topNavigation.getByRole('button', { name: 'Next receipt →' }).click();
    const secondPath = new URL(page.url()).pathname;
    await topNavigation.getByRole('button', { name: '← Previous receipt' }).click();
    assert.equal(new URL(page.url()).pathname, firstPath);
    await page.getByLabel('Select suggestion fixture-transaction', { exact: true }).check();
    await page.getByRole('button', { name: 'Approve & next →', exact: true }).first().click();
    assert.equal(new URL(page.url()).pathname, secondPath, 'approval must not skip the next receipt');
    assert.ok(new URL(page.url()).searchParams.get('queue').includes('state=needs_review'));
    await check('review-mobile-next');
    const remainingName = await page.getByRole('heading', { level: 1 }).textContent();
    await page.goto(base + '/receipts?state=needs_review&q=' + encodeURIComponent(remainingName));
    await page.locator('.receipt-name').first().click();
    await page.getByLabel('Select suggestion fixture-transaction', { exact: true }).check();
    await page.getByRole('button', { name: 'Approve & return to queue', exact: true }).first().click();
    await page.getByRole('heading', { name: 'No receipts in this view.' }).waitFor();
    await page.goto(base + '/');
    await page.getByRole('button', { name: 'Pause processing' }).click();
    await page.getByRole('button', { name: 'Resume processing' }).waitFor();
    await check('operations-mobile-paused');
    await page.getByRole('button', { name: 'Resume processing' }).click();
    await page.getByRole('button', { name: 'Pause processing' }).waitFor();
    await page.getByRole('button', { name: 'Log out' }).click();
    await page.getByRole('heading', { name: 'Welcome back' }).waitFor();
    assert.deepEqual(errors, []);
    console.log('Desktop/mobile layout, WCAG axe checks, pagination, search, correction, previous/next, approve-and-next, final-item return, pause/resume and logout passed. Screenshots: ' + screenshots);
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
