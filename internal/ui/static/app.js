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
  const webhookList = document.getElementById("webhook-list");

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

  const MONTH_NAMES = [
    "January", "February", "March", "April", "May", "June",
    "July", "August", "September", "October", "November", "December",
  ];

  function fmtTs(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return iso;
    const pad = (n) => String(n).padStart(2, "0");
    const date = `${MONTH_NAMES[d.getMonth()]} ${d.getDate()}, ${d.getFullYear()}`;
    const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    return `${date} ${time}`;
  }

  function escapeHTML(s) {
    return String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  }

  function renderSidebar() {
    channelList.setAttribute("role", "listbox");
    channelList.setAttribute("aria-label", "Channels");
    channelList.innerHTML = topics
      .map((t) => {
        const isActive = t === activeTopic;
        const active = isActive ? " active" : "";
        // Roving tabindex: only the active row is in the tab order; arrow keys
        // move focus and reassign tabindex=0 so Shift+Tab returns to the row
        // the user left on.
        const tabindex = isActive ? "0" : "-1";
        return `<li class="channel${active}" role="option" data-topic="${escapeHTML(t)}" aria-selected="${isActive}" tabindex="${tabindex}">
          <span class="hash">#</span>${escapeHTML(t)}
        </li>`;
      })
      .join("");
    for (const li of channelList.querySelectorAll("li.channel")) {
      li.addEventListener("click", () => selectChannel(li.dataset.topic));
      li.addEventListener("keydown", onChannelKeydown);
    }
  }

  function onChannelKeydown(e) {
    const items = Array.from(channelList.querySelectorAll("li.channel"));
    const idx = items.indexOf(e.currentTarget);
    if (idx < 0 || !items.length) return;
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        focusChannelItem(items[(idx + 1) % items.length]);
        break;
      case "ArrowUp":
        e.preventDefault();
        focusChannelItem(items[(idx - 1 + items.length) % items.length]);
        break;
      case "Home":
        e.preventDefault();
        focusChannelItem(items[0]);
        break;
      case "End":
        e.preventDefault();
        focusChannelItem(items[items.length - 1]);
        break;
      case "Enter":
      case " ":
        e.preventDefault();
        selectChannel(e.currentTarget.dataset.topic);
        break;
    }
  }

  function focusChannelItem(li) {
    for (const item of channelList.querySelectorAll("li.channel")) {
      item.tabIndex = -1;
    }
    li.tabIndex = 0;
    li.focus();
  }

  function selectChannel(topic) {
    if (!topic || topic === activeTopic) return;
    const wasFocused = channelList.contains(document.activeElement);
    activeTopic = topic;
    activeChannelLabel.textContent = topic;
    renderSidebar();
    if (wasFocused) {
      const newActive = channelList.querySelector("li.channel.active");
      if (newActive) newActive.focus();
    }
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
    topicsPollTimer = setInterval(() => {
      fetchTopics();
      fetchWebhooks();
    }, TOPICS_POLL_MS);
  }
  function stopTopicsPoll() {
    if (topicsPollTimer) {
      clearInterval(topicsPollTimer);
      topicsPollTimer = null;
    }
  }

  function renderWebhooks(list) {
    if (!list.length) {
      webhookList.innerHTML = `<div class="webhook empty-hint">no subscribers</div>`;
      return;
    }
    webhookList.innerHTML = list
      .map((w) => {
        const topic = w.topic ? `<span class="topic">#${escapeHTML(w.topic)}</span>` : `<span class="topic">all topics</span>`;
        return `<div class="webhook" title="id: ${escapeHTML(w.id)}">
          ${topic}
          <span class="url">${escapeHTML(w.url)}</span>
        </div>`;
      })
      .join("");
  }

  async function fetchWebhooks() {
    const token = getToken();
    if (!token) return;
    try {
      const resp = await fetch("/v1/webhooks", {
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
      renderWebhooks(list);
    } catch (err) {
      console.error("fetch webhooks failed", err);
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
    await fetchWebhooks();
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

  // Test-only hook: exposes internals when a harness sets the flag before
  // loading this script. Production pages never set it, so the global stays
  // undefined and encapsulation is preserved.
  if (typeof window !== "undefined" && window.__DINGDONG_TEST__) {
    window.__dingdong = {
      selectChannel,
      ensureTopic,
      fetchTopics,
      connect,
      bootstrap,
      stopTopicsPoll,
      state() {
        return {
          activeTopic,
          topics: topics.slice(),
          knocks: knocks.slice(),
          hasPoll: topicsPollTimer !== null,
          es,
        };
      },
    };
  }
})();
