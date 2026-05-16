# JS UI tests

Tests for `internal/ui/static/app.js` live in `tests/ui/` and run under
[vitest](https://vitest.dev) with the [happy-dom](https://github.com/capricorn86/happy-dom)
DOM implementation.

## Why vitest + happy-dom (not Playwright)

The behaviors we need to cover are pure client-side state transitions:

- channel-click → `activeTopic` update → sidebar re-render → reconnect with new topic
- incoming SSE `knock` → `ensureTopic` adds the topic to the sidebar before the next `/v1/topics` poll
- `/v1/topics` returns `401` → poll stops, auth overlay surfaces

None of those require a real browser, a real network, or a real Go server.
A jsdom-style harness with mocked `fetch` and a fake `EventSource` exercises
the same code paths, and:

- starts in well under a second (Playwright would need 10–30 s of browser boot)
- adds no browser binaries to CI (just Node)
- needs no live server, port, or fixture data

Playwright would be the right tool when we want end-to-end coverage that
includes the Go server, the SSE pipeline, and real browser rendering. The
test stack here is not a substitute for that — it's the "fast unit layer"
the UI was missing.

## Local

```sh
npm install        # one-time
npm test           # run vitest once
npm run test:watch # iterate
```

Requires Node 18+ (we test under Node 22 in CI).

## How the harness works

- `tests/ui/helpers.js` reads `internal/ui/static/app.js` from disk, sets up a
  minimal DOM mirroring the ids `app.js` reaches into, installs a
  `FakeEventSource` and a `vi.fn()` fetch mock, and evals the script via
  `new Function(code)()`.
- The script exposes a test hook at `window.__dingdong` (functions plus a
  state snapshot getter) only when `window.__DINGDONG_TEST__` is true. The
  harness sets the flag before loading; production pages never set it, so
  the global stays `undefined` and IIFE encapsulation is preserved.

## When app.js changes

If `app.js` starts touching a new DOM id, add it to `BODY_HTML` in
`tests/ui/helpers.js`. If it reaches a new internal function that a test
wants to drive, add it to the `window.__dingdong` block at the bottom of
`app.js`.
