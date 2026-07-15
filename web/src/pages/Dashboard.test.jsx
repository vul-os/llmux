/**
 * Dashboard.test.jsx — adversarial escaping at the component boundary.
 *
 * The dashboard renders strings it does NOT control: provider names, model ids,
 * and virtual-key names/tokens all arrive from the gateway's admin API. A
 * compromised or misconfigured upstream (or a hostile model id synced from a
 * third-party price catalog) must never be able to inject markup into this
 * console — the one screen that displays API keys and can reach the admin
 * surface. React escapes text children by default; there is no
 * dangerouslySetInnerHTML anywhere in this tree. THIS is the regression guard
 * that keeps it that way: if anyone ever swaps a value into innerHTML, these
 * payloads execute and the assertions below fail.
 */
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, waitFor, within, fireEvent } from "@testing-library/react";

vi.mock("../api.js", () => {
  const XSS = '<img src=x onerror="window.__xss=1">';
  return {
    getBase: () => "",
    setBase: vi.fn(),
    getMasterKey: () => "",
    setMasterKey: vi.fn(),
    api: {
      health: vi.fn().mockResolvedValue({
        status: "ok",
        providers: [{ name: `openai${XSS}`, stability: "stable" }],
      }),
      usage: vi.fn().mockResolvedValue({
        total: { requests: 3, total_tokens: 100, cost_usd: 0.5 },
        by_model: { [`gpt-4o${XSS}`]: { requests: 3, total_tokens: 100, cost_usd: 0.5 } },
      }),
      keys: vi.fn().mockResolvedValue({
        keys: [
          { name: `prod${XSS}`, key: `sk-llmux-${XSS}`, budget_usd: 100, spend_usd: 1, rpm: 60 },
        ],
      }),
      models: vi.fn().mockResolvedValue({
        data: [{ id: `evil${XSS}`, input_price_per_mtok: 1, output_price_per_mtok: 2, context_window: 128000 }],
      }),
      catalog: vi.fn().mockResolvedValue({ models: [] }),
    },
  };
});

import Dashboard from "./Dashboard.jsx";

const PAYLOAD = '<img src=x onerror="window.__xss=1">';

beforeEach(() => {
  delete window.__xss;
});

describe("gateway strings are React-escaped, never executed", () => {
  it("renders a malicious provider name as inert text in the health banner", async () => {
    const { container } = render(<Dashboard />);
    await waitFor(() => expect(container.querySelector(".banner.ok")).toBeTruthy());

    // The payload appears as literal text...
    expect(container.querySelector(".banner.ok").textContent).toContain(PAYLOAD);
    // ...and NOT as a live <img> that could fire onerror.
    expect(container.querySelector('img[src="x"]')).toBeNull();
    expect(window.__xss).toBeUndefined();
  });

  it("renders a malicious model id (usage table) as inert text", async () => {
    const { container } = render(<Dashboard />);
    await screen.findByText(`gpt-4o${PAYLOAD}`);
    expect(container.querySelector('img[src="x"]')).toBeNull();
    expect(window.__xss).toBeUndefined();
  });

  it("renders a malicious key name AND key token as inert text on the keys tab", async () => {
    const { container } = render(<Dashboard />);
    // Switch to the keys tab.
    fireEvent.click(screen.getByRole("tab", { name: "keys" }));

    await screen.findByText(`prod${PAYLOAD}`);
    // The key token lives in a <code>, still escaped.
    const code = container.querySelector("code.key-token");
    expect(code.textContent).toBe(`sk-llmux-${PAYLOAD}`);
    expect(container.querySelector('img[src="x"]')).toBeNull();
    expect(window.__xss).toBeUndefined();
  });

  it("renders a malicious model id (models tab / filter) as inert text", async () => {
    render(<Dashboard />);
    fireEvent.click(screen.getByRole("tab", { name: "models" }));

    const cell = await screen.findByText(`evil${PAYLOAD}`);
    expect(within(cell).queryByRole("img")).toBeNull();
    expect(document.querySelector('img[src="x"]')).toBeNull();
    expect(window.__xss).toBeUndefined();
  });
});
