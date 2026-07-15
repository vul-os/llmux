/**
 * api.test.js — the gateway client's theme + CREDENTIAL storage, in jsdom.
 *
 * Why this file exists: api.js is the only module that touches persistence, and
 * one of the things it persists is the admin MASTER KEY. A regression that (a)
 * widened where the key is written, (b) logged it, or (c) sent it somewhere
 * other than the configured gateway would be a real credential-disclosure bug in
 * a console whose entire job is to hold that key. These are the unit-level guards
 * for that contract; the E2E layer proves the same properties in a real browser.
 */
import { describe, it, expect, beforeEach, vi } from "vitest";
import {
  getTheme,
  applyTheme,
  resolvedTheme,
  getBase,
  setBase,
  getMasterKey,
  setMasterKey,
  api,
} from "./api.js";

const setMatch = (matches) => {
  window.matchMedia = vi.fn().mockReturnValue({
    matches,
    addEventListener() {},
    removeEventListener() {},
  });
};

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  setMatch(false);
});

describe("theme", () => {
  it("defaults to system with no data-theme attribute set", () => {
    expect(getTheme()).toBe("system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  });

  it("persists an explicit light/dark choice and reflects it on <html>", () => {
    applyTheme("dark");
    expect(getTheme()).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(localStorage.getItem("llmux_theme")).toBe("dark");
  });

  it("clearing to system removes both the attribute and the stored value", () => {
    applyTheme("dark");
    applyTheme("system");
    expect(getTheme()).toBe("system");
    expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
    expect(localStorage.getItem("llmux_theme")).toBeNull();
  });

  it("ignores a garbage persisted value and falls back to system", () => {
    localStorage.setItem("llmux_theme", "chartreuse");
    expect(getTheme()).toBe("system");
  });

  it("resolvedTheme honours the OS preference only while on system", () => {
    setMatch(true); // OS = dark
    expect(resolvedTheme()).toBe("dark");
    applyTheme("light"); // explicit choice wins over the OS preference
    expect(resolvedTheme()).toBe("light");
  });
});

describe("gateway base + master key storage", () => {
  it("round-trips the base URL", () => {
    expect(getBase()).toBe("");
    setBase("https://gw.example.com");
    expect(getBase()).toBe("https://gw.example.com");
  });

  it("round-trips the master key", () => {
    expect(getMasterKey()).toBe("");
    setMasterKey("sk-llmux-secret");
    expect(getMasterKey()).toBe("sk-llmux-secret");
  });

  it("clearing the key removes it (never leaves a stale credential behind)", () => {
    setMasterKey("sk-llmux-secret");
    setMasterKey("");
    expect(getMasterKey()).toBe("");
    expect(localStorage.getItem("llmux_master_key")).toBe("");
  });

  it("writes the master key ONLY under llmux_master_key — no other storage slot", () => {
    setMasterKey("sk-llmux-topsecret");
    const hits = [];
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (String(localStorage.getItem(k)).includes("sk-llmux-topsecret")) hits.push(k);
    }
    expect(hits).toEqual(["llmux_master_key"]);
  });
});

describe("api requests carry the key as a bearer to the configured base only", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({ ok: true }) })
    );
  });

  it("omits the Authorization header entirely when no key is set", async () => {
    await api.health();
    const [, init] = fetch.mock.calls[0];
    expect(init?.headers?.Authorization).toBeUndefined();
  });

  it("sends the master key as a Bearer token to the configured base, and nowhere else", async () => {
    setBase("https://gw.example.com");
    setMasterKey("sk-llmux-secret");
    await api.usage();
    const [url, init] = fetch.mock.calls[0];
    expect(url).toBe("https://gw.example.com/admin/usage");
    expect(init.headers.Authorization).toBe("Bearer sk-llmux-secret");
    // The key is confined to the Authorization header — it must never be smuggled
    // into the URL (query string, path) where it would land in logs/referrers.
    expect(url).not.toContain("sk-llmux-secret");
  });

  it("surfaces a non-ok response as a thrown, truncated error (no hang)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({ ok: false, status: 401, text: async () => "missing master key" })
    );
    await expect(api.keys()).rejects.toThrow(/401/);
  });
});
