(function () {
  const TOKEN_KEY = "dingdong.token";
  const DEFAULT_TOPIC = "main";
  const TOPICS_POLL_MS = 15000;

  const feed = document.getElementById("feed");
  const dot = document.getElementById("status-dot");
  const toInput = document.getElementById("to");
  const channelList = document.getElementById("channel-list");
  const activeChannelLabel = document.getElementById("active-channel");
  const overlay = document.getElementById("auth-overlay");
  const tokenInput = document.getElementById("token-input");
  const authForm = document.getElementById("auth-form");

  let es = null;
  let knocks = [];
  let topics = [DEFAULT_TOPIC];
  let activeTopic = DEFAULT_TOPIC;
  let topicsPollTimer = null;

  function getToken() {
    return localStorage.getItem(TOKEN_KEY) || "";
  }
  function setToken(t) {
    localStorage.setItem(TOKEN_KEY, t);
  }
  function forgetToken() {
    localStorage.removeItem(TOKEN_KEY);
    if (es) es.close();
    stopTopicsPoll();
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
    bootstrap();
  });

  document.getElementById("logout").addEventListener("click", forgetToken);
  document.getElementById("clear").addEventListener("click", () => {
    knocks = [];
    render();
  });
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
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  }

  function renderSidebar() {
    channelList.innerHTML = topics
      .map((t) => {
        const active = t === activeTopic ? " active" : "";
        return `<li class="channel${active}" data-topic="${escapeHTML(t)}">
          <span class="hash">#</span>${escapeHTML(t)}
        </li>`;
      })
      .join("");
    for (const li of channelList.querySelectorAll("li.channel")) {
      li.addEventListener("click", () => selectChannel(li.dataset.topic));
    }
  }

  function selectChannel(topic) {
    if (!topic || topic === activeTopic) return;
    activeTopic = topic;
    activeChannelLabel.textContent = topic;
    renderSidebar();
    connect();
  }

  function ensureTopic(topic) {
    if (!topic) return;
    if (topics.includes(topic)) return;
    topics = topics.concat([topic]).sort();
    renderSidebar();
  }

  async function fetchTopics() {
    const token = getToken();
    if (!token) return;
    try {
      const resp = await fetch("/v1/topics", {
        headers: { Authorization: "Bearer " + token },
      });
      if (resp.status === 401) {
        stopTopicsPoll();
        promptForToken();
        return;
      }
      if (!resp.ok) return;
      const list = await resp.json();
      if (!Array.isArray(list)) return;
      // A knock may arrive over SSE and be added via ensureTopic() before
      // the next poll catches up — merge so the sidebar stays consistent.
      const merged = new Set(list);
      merged.add(DEFAULT_TOPIC);
      for (const t of topics) merged.add(t);
      topics = Array.from(merged).sort();
      renderSidebar();
    } catch (err) {
      console.error("fetch topics failed", err);
    }
  }

  function startTopicsPoll() {
    stopTopicsPoll();
    topicsPollTimer = setInterval(fetchTopics, TOPICS_POLL_MS);
  }
  function stopTopicsPoll() {
    if (topicsPollTimer) {
      clearInterval(topicsPollTimer);
      topicsPollTimer = null;
    }
  }

  function render() {
    if (!knocks.length) {
      feed.innerHTML = `<div class="empty">no knocks in #${escapeHTML(activeTopic)} yet</div>`;
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
    params.set("topic", activeTopic);
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
        ensureTopic(k.topic);
        render();
      } catch (err) {
        console.error("bad knock payload", err, ev.data);
      }
    });
  }

  async function bootstrap() {
    activeChannelLabel.textContent = activeTopic;
    renderSidebar();
    await fetchTopics();
    startTopicsPoll();
    connect();
  }

  // Bookmark shortcut: visiting `/?token=XXX` absorbs the token into
  // localStorage and strips it from the URL bar so it isn't left in
  // browser history. Once stashed, the token persists across reloads
  // and browser restarts.
  const urlToken = new URLSearchParams(window.location.search).get("token");
  if (urlToken) {
    setToken(urlToken);
    const cleaned = new URL(window.location.href);
    cleaned.searchParams.delete("token");
    window.history.replaceState({}, "", cleaned.pathname + cleaned.search + cleaned.hash);
  }

  if (!getToken()) promptForToken();
  else bootstrap();
})();
