import { lazy, Suspense } from "react";
import { Routes, Route, NavLink, Link } from "react-router-dom";
import { Logo } from "./components/Logo.jsx";
import Landing from "./pages/Landing.jsx";

// Code-split: the landing (entry) stays light; markdown/highlight + dashboard
// load only when their routes are visited.
const Docs = lazy(() => import("./pages/Docs.jsx"));
const Dashboard = lazy(() => import("./pages/Dashboard.jsx"));

export default function App() {
  return (
    <div className="app">
      <header className="topbar">
        <div className="wrap">
          <Link to="/" aria-label="llmux home"><Logo /></Link>
          <nav className="nav">
            <NavLink to="/" end className="hide-sm">Home</NavLink>
            <NavLink to="/docs">Docs</NavLink>
            <NavLink to="/app">Dashboard</NavLink>
            <a className="hide-sm" href="https://github.com/llmux/llmux" target="_blank" rel="noreferrer">GitHub</a>
            <a className="btn ghost-btn" href="https://llmux.to" target="_blank" rel="noreferrer">Cloud →</a>
          </nav>
        </div>
      </header>

      <Suspense fallback={<div className="wrap" style={{ padding: "120px 0", color: "var(--muted)", fontFamily: "var(--font-mono)" }}>loading…</div>}>
        <Routes>
          <Route path="/" element={<Landing />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:slug" element={<Docs />} />
          <Route path="/app" element={<Dashboard />} />
          <Route path="*" element={<Landing />} />
        </Routes>
      </Suspense>

      <footer className="footer">
        <div className="wrap">
          <span>llmux — the LLM multiplexer · MIT · one gateway, every provider, every language</span>
          <span><a href="/docs">Docs</a> · <a href="https://github.com/llmux/llmux">GitHub</a> · <a href="https://llmux.to">Cloud</a></span>
        </div>
      </footer>
    </div>
  );
}
