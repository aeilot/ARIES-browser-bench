// Records the browser's network and page-load timing as JSON lines for ARIES
// (see timing.go). Playwright MCP's --init-page loads this module and calls
// its default export once for every page it opens. Recording must never
// break browsing, so every failure here is swallowed.
'use strict';

const fs = require('fs');

const output = process.env.ARIES_NETWORK_TIMING_FILE || '/tmp/aries-browser/network.jsonl';
const maxURLLength = 2048;
let pages = 0;

function write(record) {
  try {
    fs.appendFileSync(output, JSON.stringify(record) + '\n');
  } catch (error) {
    // Evidence is best effort; the agent's browsing is not.
  }
}

function mainFrameRequest(page, request) {
  try {
    return request.frame() === page.mainFrame();
  } catch (error) {
    return false; // service worker requests have no frame
  }
}

async function recordRequest(page, pageID, request, failure) {
  const endMs = Date.now();
  const timing = request.timing();
  const record = {
    type: 'request',
    page: pageID,
    url: request.url().slice(0, maxURLLength),
    method: request.method(),
    resource_type: request.resourceType(),
    navigation: request.isNavigationRequest(),
    main_frame: mainFrameRequest(page, request),
    // start_ms is epoch milliseconds; the phase offsets are milliseconds
    // after it, or -1 when the phase did not happen (a reused connection,
    // a failed request).
    start_ms: timing.startTime,
    end_ms: endMs,
    timing: {
      dns_start: timing.domainLookupStart,
      dns_end: timing.domainLookupEnd,
      connect_start: timing.connectStart,
      tls_start: timing.secureConnectionStart,
      connect_end: timing.connectEnd,
      request_start: timing.requestStart,
      response_start: timing.responseStart,
      response_end: timing.responseEnd,
    },
  };
  if (failure) {
    record.failure = failure;
  } else {
    try {
      const response = await request.response();
      if (response) record.status = response.status();
    } catch (error) {}
    try {
      const sizes = await request.sizes();
      record.sizes = {
        request_headers: sizes.requestHeadersSize,
        request_body: sizes.requestBodySize,
        response_headers: sizes.responseHeadersSize,
        response_body: sizes.responseBodySize,
      };
    } catch (error) {}
  }
  write(record);
}

async function recordLoad(page, pageID) {
  const loadMs = Date.now();
  try {
    const navigation = await page.evaluate(() => {
      const entry = performance.getEntriesByType('navigation')[0];
      if (!entry) return null;
      return {
        time_origin: performance.timeOrigin,
        request_start: entry.requestStart,
        response_start: entry.responseStart,
        response_end: entry.responseEnd,
        dom_content_loaded_end: entry.domContentLoadedEventEnd,
        load_end: entry.loadEventEnd,
        transfer_size: entry.transferSize,
      };
    });
    write({ type: 'load', page: pageID, url: page.url().slice(0, maxURLLength), event_ms: loadMs, navigation_timing: navigation });
  } catch (error) {
    write({ type: 'load', page: pageID, url: page.url().slice(0, maxURLLength), event_ms: loadMs, navigation_timing: null });
  }
}

exports.default = async ({ page }) => {
  const pageID = ++pages;
  write({ type: 'page', page: pageID, event_ms: Date.now() });
  page.on('requestfinished', (request) => { recordRequest(page, pageID, request, null); });
  page.on('requestfailed', (request) => {
    const failure = request.failure();
    recordRequest(page, pageID, request, (failure && failure.errorText) || 'failed');
  });
  page.on('load', () => { recordLoad(page, pageID); });
};
