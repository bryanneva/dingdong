import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { vi } from "vitest";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(__dirname, "..", "..");
const APP_JS = fs.readFileSync(
  path.join(REPO_ROOT, "internal/ui/static/app.js"),
  "utf8",
);

// Minimal DOM that mirrors the ids and structure app.js reaches into.
// Kept in sync with internal/ui/static/index.html by hand — if app.js starts
// touching a new element, add it here.
const BODY_HTML = `
  <header>
    <span class="dot" id="status-dot"></span>
    <span id="active-channel">main</span>
    <input id="to" type="text" />
    <button id="clear">clear</button>
    <button id="logout">forget token</button>
  </header>
  <aside id="sidebar"><ul id="channel-list"></ul></aside>
  <main id="feed"></main>
  <div class="auth-overlay" id="auth-overlay" hidden>
    <form id="auth-form">
      <input id="token-input" type="password" />
      <button type="submit">connect</button>
    </form>
  </div>
`;

export class FakeEventSource {
  static instances = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;

  constructor(url) {
    this.url = url;
    this.readyState = FakeEventSource.CONNECTING;
    this.listeners = {};
    this.closed = false;
    FakeEventSource.instances.push(this);
  }

  addEventListener(name, cb) {
    (this.listeners[name] ||= []).push(cb);
  }

  dispatch(name, data) {
    for (const cb of this.listeners[name] || []) {
      cb({ data });
    }
  }

  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }

  static reset() {
    FakeEventSource.instances = [];
  }
}

export function resetEnvironment() {
  document.body.innerHTML = BODY_HTML;
  window.localStorage.clear();
  FakeEventSource.reset();
  window.__DINGDONG_TEST__ = true;
  window.__dingdong = undefined;
  globalThis.EventSource = FakeEventSource;
  globalThis.fetch = vi.fn();
}

export function loadAppScript() {
  // eslint-disable-next-line no-new-func
  new Function(APP_JS)();
}

export function mockTopicsResponse(list) {
  globalThis.fetch.mockResolvedValueOnce({
    status: 200,
    ok: true,
    json: async () => list,
  });
}

export function mockTopicsStatus(status) {
  globalThis.fetch.mockResolvedValueOnce({
    status,
    ok: status >= 200 && status < 300,
    json: async () => ({}),
  });
}
