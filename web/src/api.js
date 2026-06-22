// API client for the gateway. Same-origin by default (the dashboard is served
// from the gateway at /ui), with an optional override for a remote gateway.
// The admin master key is kept in localStorage and sent as a bearer token.

const BASE_KEY = "llmux_api_base";
const MASTER_KEY = "llmux_master_key";
const THEME_KEY = "llmux_theme";

// Theme: an explicit "light"/"dark" choice is persisted and reflected on
// <html data-theme>. With no saved choice we return "system" and leave the
// attribute off so prefers-color-scheme drives the palette (see styles.css).
export function getTheme() {
  const t = localStorage.getItem(THEME_KEY);
  return t === "light" || t === "dark" ? t : "system";
}
export function applyTheme(theme) {
  const root = document.documentElement;
  if (theme === "light" || theme === "dark") {
    root.setAttribute("data-theme", theme);
    localStorage.setItem(THEME_KEY, theme);
  } else {
    root.removeAttribute("data-theme");
    localStorage.removeItem(THEME_KEY);
  }
}
// Effective light/dark, resolving "system" against the OS preference.
export function resolvedTheme() {
  const t = getTheme();
  if (t !== "system") return t;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function getBase() {
  return localStorage.getItem(BASE_KEY) || "";
}
export function setBase(v) {
  localStorage.setItem(BASE_KEY, v || "");
}
export function getMasterKey() {
  return localStorage.getItem(MASTER_KEY) || "";
}
export function setMasterKey(v) {
  localStorage.setItem(MASTER_KEY, v || "");
}

async function get(path) {
  const headers = {};
  const key = getMasterKey();
  if (key) headers["Authorization"] = "Bearer " + key;
  const res = await fetch(getBase() + path, { headers });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(`${res.status}: ${text.slice(0, 200)}`);
  }
  return res.json();
}

export const api = {
  health: () => get("/health"),
  usage: () => get("/admin/usage"),
  keys: () => get("/admin/keys"),
  models: () => get("/v1/models"),
  catalog: () => get("/v1/catalog.json"),
};
