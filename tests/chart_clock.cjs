// Requires Node 20+, Playwright and Chromium. All admin data is intercepted;
// this test never creates accounts, keys, or inference requests in the service.
const assert = require('node:assert/strict');
const {chromium} = require('playwright');

const base = process.env.TEST_URL || 'http://127.0.0.1:18002';
const epoch = Date.parse('2026-10-29T12:00:00.000Z');
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));

async function eventually(check, description) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await pause(20);
  }
  throw Error('Timed out: ' + description);
}

function distribution(samples, value) {
  return {samples, mean: samples ? value : null, p50: samples ? value : null,
    p95: samples ? value : null, bins: [{lower: 0, upper: null, count: samples}]};
}

async function fixture(browser, {ages = [20000], samples = [], period = '0.016666666666666666', skew = 0, failExpired = 0} = {}) {
  const context = await browser.newContext({viewport: {width: 1440, height: 1100}, timezoneId: 'UTC'});
  const page = await context.newPage();
  const errors = [], reads = [];
  let serverNow = epoch;
  const records = ages.map((age, i) => ({stamp: epoch - age, rate: 10 + i * 10, samples: samples[i] || 1}));
  page.on('pageerror', error => errors.push(error.message));
  await page.clock.install({time: new Date(epoch + skew)});
  await context.addInitScript(({period}) => {
    localStorage.setItem('model-meter.period', period);
    // Keep the real app's SSE consumer, but supply a connection with no change
    // events. Heartbeats alone must never trigger HTTP data polling.
    const realFetch = window.fetch.bind(window);
    window.fetch = (input, options) => {
      if (new URL(typeof input === 'string' ? input : input.url, location.href).pathname !== '/admin/events') return realFetch(input, options);
      const encoder = new TextEncoder();
      let heartbeat;
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(encoder.encode('event: ready\ndata: {}\n\n'));
          heartbeat = setInterval(() => controller.enqueue(encoder.encode('event: heartbeat\ndata: {}\n\n')), 15000);
          options?.signal?.addEventListener('abort', () => {
            clearInterval(heartbeat);
            controller.error(new DOMException('Aborted', 'AbortError'));
          }, {once: true});
        },
        cancel() { clearInterval(heartbeat); }
      });
      return Promise.resolve(new Response(body, {headers: {'Content-Type': 'text/event-stream'}}));
    };
  }, {period});
  await page.route('**/admin/**', async route => {
    const url = new URL(route.request().url());
    reads.push(url.pathname + url.search);
    const hours = await page.locator('#period').inputValue();
    const span = Number(hours) * 3600000;
    // The response is authoritative even when this browser's wall clock is
    // wrong. That isolates rendering from client-side query construction.
    const start = span ? serverNow - span : Math.min(serverNow - 1000, ...records.map(r => r.stamp));
    const included = records.filter(r => r.stamp > start || (!span && r.stamp === start));
    if (url.pathname === '/admin/performance' && !included.length && failExpired > 0) {
      failExpired--;
      await route.fulfill({status: 503, json: {error: {message: 'Temporarily unavailable'}}});
      return;
    }
    const seconds = Math.max(1, (serverNow - start) / 1000);
    const bucket = [1, 5, 10, 30, 60, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400].find(value => seconds / value <= 120) || 86400;
    const points = included.map(r => ({time: new Date(Math.floor(r.stamp / 1000 / bucket) * bucket * 1000).toISOString(),
      requests: r.samples, input_tokens: 100 * r.samples, output_tokens: 20 * r.samples, output_tps: r.rate, rate_samples: r.samples}));
    const values = {
      '/admin/me': {id: 1, name: 'Clock fixture', username: 'clock', admin: true},
      '/admin/keys': [], '/admin/users': [], '/admin/status': {dropped_records: 0, queued_records: 0},
      '/admin/settings': {allow_anonymous: false},
      '/admin/usage': included.length ? [{key_id: null, name: 'Anonymous', model: 'fixture', requests: included.length,
        input_tokens: included.length * 100, output_tokens: included.length * 20, cached_tokens: 0, errors: 0, unmetered: 0, avg_ms: 2000}] : [],
      '/admin/requests': included.map(r => ({time: new Date(r.stamp).toISOString(), key_name: 'Anonymous', model: 'fixture', method: 'POST',
        path: '/v1/chat/completions', source: '127.0.0.1', status: 200, input_tokens: 100, output_tokens: 20, duration_ms: 2000, outcome: 'complete'})),
      '/admin/performance': {start: new Date(start).toISOString(), end: new Date(serverNow).toISOString(), bucket_seconds: bucket,
        expires_at: span && included.length ? new Date(Math.min(...included.map(r => r.stamp)) + span).toISOString() : null,
        latency: distribution(included.length, 2), output_rate: distribution(included.length, 10), series: points}
    };
    assert.ok(url.pathname in values, 'Unexpected admin request: ' + url.pathname);
    await route.fulfill({json: values[url.pathname]});
  });
  await page.route('**/auth/logout', route => route.fulfill({json: {}}));
  await page.route('**/v1/**', () => { throw Error('Chart test must not send inference requests'); });
  await page.goto(base);
  await eventually(async () => await page.locator('#dashboard').isVisible() && await page.locator('#requestChart svg').count() === 1, 'dashboard restored');
  // Let the ready event's initial snapshot finish before stopping wall time.
  let previous;
  do { previous = reads.length; await pause(80); } while (reads.length !== previous);
  await page.clock.pauseAt(new Date(epoch + skew + 1000));
  serverNow += 1000;
  return {
    page, reads, errors,
    async advance(ms) { serverNow += ms; await page.clock.runFor(ms); await pause(30); },
    async close() { assert.deepEqual(errors, [], 'No browser script errors'); await context.close(); },
    async view(name) { await page.locator('[data-view="' + name + '"]').click(); },
    async settled() { let before; do { before = reads.length; await pause(80); } while (before !== reads.length); },
    async coordinates(id = 'requestChart') { return page.locator('#' + id + ' circle').evaluateAll(nodes => nodes.map(node => Number(node.getAttribute('cx')))); }
  };
}

(async () => {
  const browser = await chromium.launch({headless: true, args: ['--no-sandbox']});
  const checks = [];
  try {
    {
      const f = await fixture(browser);
      const before = await Promise.all(['requestChart', 'tokenChart', 'speedChart'].map(id => f.coordinates(id)));
      const reads = f.reads.length;
      await f.page.evaluate(() => {
        window.histogramChanges = 0;
        const observer = new MutationObserver(() => window.histogramChanges++);
        observer.observe(document.getElementById('latencyHistogram'), {childList: true, subtree: true});
      });
      await f.advance(4000);
      for (const [i, id] of ['requestChart', 'tokenChart', 'speedChart'].entries()) {
        const after = await f.coordinates(id);
        assert.equal(after.length, before[i].length, id + ' retains its samples');
        assert.ok(before[i][0] - after[0] > 20, id + ' moves left without a new request');
      }
      assert.equal(f.reads.length, reads, 'Local chart ticks do not fetch data');
      assert.equal(await f.page.evaluate(() => window.histogramChanges), 0, 'Local ticks leave unchanged histograms alone');
      await f.advance(12000);
      assert.equal(f.reads.length, reads, 'SSE heartbeat does not fetch data');
      await f.close(); checks.push('all three timelines move locally without HTTP polling');
    }
    {
      const f = await fixture(browser, {ages: [55000]});
      const reads = f.reads.length;
      await f.advance(2000);
      assert.equal(f.reads.length, reads, 'No fetch before the oldest sample expires');
      await f.advance(4000);
      await eventually(async () => await f.page.locator('#requests').textContent() === '0', 'expired totals refreshed');
      assert.ok(f.reads.length > reads, 'Expiry fetches a fresh snapshot');
      assert.equal((await f.coordinates()).length, 0, 'Expired request leaves the timeline');
      assert.match(await f.page.locator('#latencySummary').textContent(), /^0 samples/, 'Expired request leaves histogram');
      const afterExpiry = f.reads.length;
      await f.advance(16000);
      assert.equal(f.reads.length, afterExpiry, 'Empty windows do not keep fetching');
      await f.close(); checks.push('known expiry refreshes totals and histograms once');
    }
    {
      const f = await fixture(browser, {ages: [55000], failExpired: 1});
      const performanceReads = () => f.reads.filter(path => path.startsWith('/admin/performance?')).length;
      const before = performanceReads();
      await f.advance(6000);
      await eventually(async () => (await f.page.locator('#message').textContent()).includes('Temporarily unavailable'), 'transient expiry failure');
      assert.equal(performanceReads(), before + 1, 'First expiry snapshot failed once');
      await f.advance(1000);
      assert.equal(performanceReads(), before + 1, 'Failed expiry does not retry in a tight loop');
      await f.advance(1200);
      await eventually(async () => await f.page.locator('#requests').textContent() === '0', 'expiry retry recovers without an SSE change');
      assert.equal(performanceReads(), before + 2, 'Transient failure triggers one successful retry');
      assert.match(await f.page.locator('#latencySummary').textContent(), /^0 samples/);
      const recoveredReads = f.reads.length;
      await f.advance(16000);
      assert.equal(f.reads.length, recoveredReads, 'Recovered empty window stops fetching');
      await f.close(); checks.push('transient expiry failures retry with backoff and recover without SSE changes');
    }
    {
      const f = await fixture(browser, {ages: [55000]});
      await f.view('requests');
      const reads = f.reads.length, frozen = await f.page.locator('#requestChart').innerHTML();
      await f.advance(2000);
      assert.equal(await f.page.locator('#requestChart').innerHTML(), frozen, 'Requests view does not redraw hidden charts');
      assert.equal(f.reads.length, reads, 'Requests view does not poll before expiry');
      await f.advance(4000);
      await eventually(async () => !(await f.page.locator('#requestRows').textContent()).includes('/v1/chat/completions'), 'request feed expiry');
      assert.ok(f.reads.length > reads, 'Request feed expires at the known deadline');
      await f.close(); checks.push('request feed expires without drawing hidden charts or polling');
    }
    {
      const f = await fixture(browser, {skew: 6 * 3600000});
      const before = await f.coordinates();
      assert.equal(before.length, 1, 'Server snapshot remains visible with a skewed browser clock');
      await f.advance(4000);
      const after = await f.coordinates();
      assert.ok(before[0] - after[0] > 20, 'Server time advances using elapsed time');
      await f.page.clock.setSystemTime(new Date(epoch + 48 * 3600000));
      await f.advance(2000);
      const jumped = await f.coordinates();
      assert.equal(jumped.length, 1, 'A wall-clock correction cannot discard chart history');
      assert.ok(after[0] - jumped[0] > 5 && after[0] - jumped[0] < 30, 'Wall-clock correction advances only elapsed seconds');
      await f.close(); checks.push('server time and monotonic elapsed time tolerate clock skew');
    }
    {
      const f = await fixture(browser, {ages: [10000]});
      await f.view('access');
      const frozen = await f.page.locator('#requestChart').innerHTML(), reads = f.reads.length;
      await f.advance(8000);
      assert.equal(await f.page.locator('#requestChart').innerHTML(), frozen, 'Access view suspends chart rendering');
      assert.equal(f.reads.length, reads, 'Access view does not poll');
      await f.view('overview');
      assert.notEqual(await f.page.locator('#requestChart').innerHTML(), frozen, 'Returning to overview catches up immediately');
      await f.page.evaluate(() => {
        Object.defineProperty(document, 'hidden', {configurable: true, value: true});
        document.dispatchEvent(new Event('visibilitychange'));
      });
      const hidden = await f.page.locator('#requestChart').innerHTML();
      await f.advance(8000);
      assert.equal(await f.page.locator('#requestChart').innerHTML(), hidden, 'Hidden tab suspends chart rendering');
      await f.page.evaluate(() => {
        Object.defineProperty(document, 'hidden', {configurable: true, value: false});
        document.dispatchEvent(new Event('visibilitychange'));
      });
      await f.settled();
      assert.notEqual(await f.page.locator('#requestChart').innerHTML(), hidden, 'Visible tab catches up');
      await f.page.locator('#logout').click();
      await eventually(() => f.page.locator('#login').isVisible(), 'logout finishes');
      const loggedOut = f.reads.length;
      await f.advance(4000);
      assert.equal(f.reads.length, loggedOut, 'Logout stops clock-driven data work');
      await f.close(); checks.push('access, hidden tabs, and logout suspend work');
    }
    {
      const f = await fixture(browser, {ages: [40000, 20000]});
      const path = await f.page.locator('#speedChart path').first().getAttribute('d');
      assert.equal((path.match(/M/g) || []).length, 2, 'Unobserved speed remains a gap instead of zero');
      const reads = f.reads.length;
      await f.page.selectOption('#period', '0.08333333333333333');
      await eventually(() => Promise.resolve(f.reads.length > reads), 'new period requests a snapshot');
      await f.settled();
      assert.match(await f.page.locator('#requestChart svg').getAttribute('aria-label'), /Last 5 minutes/);
      const before = await f.coordinates();
      await f.advance(4000);
      const after = await f.coordinates();
      assert.ok(before[0] - after[0] > 2 && before[0] - after[0] < 20, 'Movement follows the new window scale');
      assert.equal(await f.page.evaluate(() => localStorage.getItem('model-meter.period')), '0.08333333333333333');
      await f.close(); checks.push('speed gaps and period changes remain accurate');
    }
    {
      const f = await fixture(browser, {ages: [90000, 30000], period: '0'});
      const initial = await f.coordinates(), reads = f.reads.length;
      assert.equal(initial.length, 2);
      await f.advance(300000);
      const after = await f.coordinates();
      assert.equal(after.length, 2, 'All-time keeps old observations as its span grows');
      assert.ok(after[1] < initial[1], 'All-time widens toward the advancing present');
      assert.equal(f.reads.length, reads, 'All-time does not poll to advance');
      const path = await f.page.locator('#requestChart path').first().getAttribute('d');
      assert.ok((path.match(/[ML]/g) || []).length <= 124, 'All-time rendering stays bounded');
      await f.close(); checks.push('all-time grows, retains history, and bounds chart complexity');
    }
    {
      const f = await fixture(browser, {ages: [10000, 8000], samples: [1, 3], period: '0'});
      assert.equal((await f.coordinates('speedChart')).length, 2);
      await f.advance(300000);
      assert.equal((await f.coordinates('speedChart')).length, 1, 'Expanding all-time merges adjacent buckets');
      assert.match(await f.page.locator('#speedChart circle title').textContent(), /17\.5 output tok\/s \/ 4 requests/,
        'Merged speed uses sample-weighted means');
      await f.close(); checks.push('expanded buckets preserve weighted output speed');
    }
    console.log(JSON.stringify({result: 'passed', checks}));
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
