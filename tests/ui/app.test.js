import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  FakeEventSource,
  loadAppScript,
  mockTopicsResponse,
  mockTopicsStatus,
  mockWebhooksResponse,
  resetEnvironment,
} from "./helpers.js";

const TOKEN = "testtoken";

beforeEach(() => {
  resetEnvironment();
  window.localStorage.setItem("dingdong.token", TOKEN);
});

afterEach(() => {
  // Stop any setInterval/EventSource the IIFE armed so it doesn't bleed into
  // later tests or keep Vitest alive after the suite exits.
  window.__dingdong?.stopTopicsPoll();
  for (const es of FakeEventSource.instances) es.close();
});

async function waitForBootstrap() {
  // bootstrap() is async: fetchTopics() → startTopicsPoll() → connect().
  // connect() opens our FakeEventSource synchronously; wait for that.
  await vi.waitFor(() => {
    if (FakeEventSource.instances.length === 0) {
      throw new Error("waiting for initial EventSource");
    }
  });
}

describe("app.js channel + SSE + auth behavior", () => {
  it("clicking a channel updates activeTopic, re-renders, and reconnects", async () => {
    mockTopicsResponse(["main", "ops"]);
    mockWebhooksResponse([]);
    loadAppScript();
    await waitForBootstrap();

    expect(window.__dingdong.state().activeTopic).toBe("main");
    const initialEs = FakeEventSource.instances[0];
    expect(initialEs.url).toContain("topic=main");

    const items = [...document.querySelectorAll("li.channel")];
    const opsLi = items.find((li) => li.dataset.topic === "ops");
    expect(opsLi, "ops channel should be in the sidebar").toBeTruthy();

    opsLi.click();

    expect(window.__dingdong.state().activeTopic).toBe("ops");
    expect(document.getElementById("active-channel").textContent).toBe("ops");
    const activeLi = document.querySelector("li.channel.active");
    expect(activeLi.dataset.topic).toBe("ops");

    // A new EventSource was opened against the new topic; the old one was closed.
    expect(FakeEventSource.instances.length).toBe(2);
    expect(initialEs.closed).toBe(true);
    const newEs = FakeEventSource.instances[1];
    expect(newEs.url).toContain("topic=ops");
  });

  it("incoming SSE knock on a new topic adds it to the sidebar via ensureTopic", async () => {
    mockTopicsResponse(["main"]);
    mockWebhooksResponse([]);
    loadAppScript();
    await waitForBootstrap();

    expect(window.__dingdong.state().topics).toEqual(["main"]);
    const sidebarBefore = [...document.querySelectorAll("li.channel")].map(
      (li) => li.dataset.topic,
    );
    expect(sidebarBefore).toEqual(["main"]);

    const es = FakeEventSource.instances[0];
    es.dispatch(
      "knock",
      JSON.stringify({
        id: "k1",
        from: "laptop:claude:demo",
        topic: "alerts",
        kind: "info",
        ts: "2026-01-01T00:00:00Z",
      }),
    );

    // ensureTopic adds 'alerts' to the topic list and re-renders the sidebar
    // before the next /v1/topics poll runs — the merge in fetchTopics is the
    // catch-up, not the source of truth.
    expect(window.__dingdong.state().topics).toContain("alerts");
    const sidebarAfter = [...document.querySelectorAll("li.channel")].map(
      (li) => li.dataset.topic,
    );
    expect(sidebarAfter).toContain("alerts");
    // fetch was only called for the initial bootstrap (one /v1/topics +
    // one /v1/webhooks), proving the sidebar update did not require another
    // /v1/topics roundtrip.
    expect(globalThis.fetch).toHaveBeenCalledTimes(2);
  });

  it("/v1/topics returning 401 stops the poll and shows the auth overlay", async () => {
    // First call (bootstrap) succeeds so the poll starts.
    mockTopicsResponse(["main"]);
    mockWebhooksResponse([]);
    loadAppScript();
    await waitForBootstrap();

    expect(window.__dingdong.state().hasPoll).toBe(true);
    expect(document.getElementById("auth-overlay").hidden).toBe(true);

    // Next /v1/topics returns 401: poll stops, overlay surfaces.
    mockTopicsStatus(401);
    await window.__dingdong.fetchTopics();

    expect(window.__dingdong.state().hasPoll).toBe(false);
    expect(document.getElementById("auth-overlay").hidden).toBe(false);
  });
});
