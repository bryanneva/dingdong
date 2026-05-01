(function () {
  const TOKEN_KEY = "dingdong.token";
  const feed = document.getElementById("feed");
  const dot = document.getElementById("status-dot");
  const topicInput = document.getElementById("topic");
  const toInput = document.getElementById("to");
  const overlay = document.getElementById("auth-overlay");
  const tokenInput = document.getElementById("token-input");
  const authForm = document.getElementById("auth-form");

  let es = null;
  let knocks = [];

  function getToken() {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  }
  function setToken(t) {
    sessionStorage.setItem(TOKEN_KEY, t);
  }
  function forgetToken() {
    sessionStorage.removeItem(TOKEN_KEY);
    if (es) es.close();
    promptForToken();
  }
  function promptForToken() {
    overlay.hidden = false;
    tokenInput.focus();
  }

  authForm.addEventListener("submit", (e) => {
    e.preventDefault();
    const t = tokenInput.value.trim();
    if (!t) return;
    setToken(t);
    overlay.hidden = true;
    tokenInput.value = "";
    connect();
  });

  document.getElementById("logout").addEventListener("click", forgetToken);
  document.getElementById("clear").addEventListener("click", () => {
    knocks = [];
    render();
  });
  topicInput.addEventListener("change", connect);
  toInput.addEventListener("change", connect);

  function setStatus(live, title) {
    dot.classList.toggle("live", !!live);
    dot.title = title || (live ? "live" : "disconnected");
  }

  function fmtTs(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return iso;
    const pad = (n) => String(n).padStart(2, "0");
    return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  }

  function escapeHTML(s) {
    return String(s ?? "").replace(/[&<>]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));
  }

  function render() {
    if (!knocks.length) {
      feed.innerHTML = '<div class="empty">no knocks yet</div>';
      return;
    }
    feed.innerHTML = knocks
      .slice()
      .reverse()
      .map((k) => {
        const kind = k.kind || "info";
        const arrow = k.to ? `<span class="arrow">→</span>${escapeHTML(k.to)}` : "";
        const subject = k.subject ? `<div class="subject">${escapeHTML(k.subject)}</div>` : "";
        const body = k.body ? `<div class="body">${escapeHTML(k.body)}</div>` : "";
        return `
          <div class="knock">
            <span class="kind kind-${escapeHTML(kind)}">${escapeHTML(kind)}</span>
            <div class="from">
              <strong>${escapeHTML(k.from)}</strong>${arrow}
              · <em>#${escapeHTML(k.topic)}</em>
            </div>
            <span class="ts">${fmtTs(k.ts)}</span>
            ${subject}
            ${body}
          </div>`;
      })
      .join("");
  }

  function connect() {
    if (es) es.close();
    const token = getToken();
    if (!token) {
      promptForToken();
      return;
    }
    knocks = [];
    render();
    setStatus(false, "connecting");

    const params = new URLSearchParams();
    params.set("token", token);
    if (topicInput.value.trim()) params.set("topic", topicInput.value.trim());
    if (toInput.value.trim()) params.set("to", toInput.value.trim());

    es = new EventSource("/v1/stream?" + params.toString());
    es.addEventListener("open", () => setStatus(true, "live"));
    es.addEventListener("error", () => {
      setStatus(false, "reconnecting");
      // EventSource auto-reconnects; if the token is wrong we'll get 401 repeatedly.
      // Detect that by readyState and prompt again.
      if (es && es.readyState === EventSource.CLOSED) {
        promptForToken();
      }
    });
    es.addEventListener("knock", (ev) => {
      try {
        const k = JSON.parse(ev.data);
        knocks.push(k);
        if (knocks.length > 500) knocks = knocks.slice(-500);
        render();
      } catch (err) {
        console.error("bad knock payload", err, ev.data);
      }
    });
  }

  if (!getToken()) promptForToken();
  else connect();
})();
