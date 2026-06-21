import { lazy, Suspense } from "react";
import { Routes, Route, NavLink, Link } from "react-router-dom";
import { Logo } from "./components/Logo.jsx";
import Landing from "./pages/Landing.jsx";

// Code-split: the landing (entry) stays light; markdown/highlight + dashboard
// load only when their routes are visited.
const Docs = lazy(() => import("./pages/Docs.jsx"));
const Dashboard = lazy(() => import("./pages/Dashboard.jsx"));

const GH = "https://github.com/llmux/llmux";

function GitHubIcon() {
  return (
    <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true">
      <path d="M12 .5C5.7.5.5 5.7.5 12c0 5.1 3.3 9.4 7.9 10.9.6.1.8-.2.8-.5v-2c-3.2.7-3.9-1.4-3.9-1.4-.5-1.3-1.3-1.7-1.3-1.7-1.1-.7.1-.7.1-.7 1.2.1 1.8 1.2 1.8 1.2 1 1.8 2.7 1.3 3.4 1 .1-.8.4-1.3.8-1.6-2.6-.3-5.3-1.3-5.3-5.8 0-1.3.5-2.3 1.2-3.1-.1-.3-.5-1.5.1-3.1 0 0 1-.3 3.3 1.2a11.5 11.5 0 0 1 6 0C17.3 4.7 18.3 5 18.3 5c.6 1.6.2 2.8.1 3.1.8.8 1.2 1.8 1.2 3.1 0 4.5-2.7 5.5-5.3 5.8.4.4.8 1.1.8 2.2v3.3c0 .3.2.6.8.5 4.6-1.5 7.9-5.8 7.9-10.9C23.5 5.7 18.3.5 12 .5Z" />
    </svg>
  );
}

export default function App() {
  return (
    <div className="app">
      <header className="topbar">
        <div className="topbar-rail" aria-hidden="true" />
        <div className="wrap topbar-inner">
          <Link to="/" aria-label="llmux home" className="brand">
            <Logo />
            <span className="brand-badge">v0.1 · MIT</span>
          </Link>

          <nav className="nav" aria-label="Primary">
            <div className="nav-seg">
              <NavLink to="/" end>Home</NavLink>
              <NavLink to="/docs">Docs</NavLink>
              <NavLink to="/app">Dashboard</NavLink>
            </div>
            <a className="nav-icon hide-sm" href={GH} target="_blank" rel="noreferrer" aria-label="GitHub">
              <GitHubIcon /><span>Star</span>
            </a>
            <a className="btn primary sm" href="https://llmux.to" target="_blank" rel="noreferrer">
              Cloud <span aria-hidden="true">→</span>
            </a>
          </nav>
        </div>
      </header>

      <Suspense fallback={<div className="route-loading"><span className="spinner" /> loading…</div>}>
        <Routes>
          <Route path="/" element={<Landing />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:slug" element={<Docs />} />
          <Route path="/app" element={<Dashboard />} />
          <Route path="*" element={<Landing />} />
        </Routes>
      </Suspense>

      <footer className="footer">
        <div className="wrap footer-inner">
          <div className="footer-brand">
            <Logo />
            <p>One OpenAI-compatible gateway for every provider, in every language. A single Go binary — routing, fallbacks, budgets, caching, and live cost.</p>
            <span className="status"><span className="status-dot" /> open source · self-host free forever</span>
          </div>
          <div className="footer-cols">
            <div className="footer-col">
              <h4>Product</h4>
              <Link to="/docs">Documentation</Link>
              <Link to="/app">Dashboard</Link>
              <a href="https://llmux.to" target="_blank" rel="noreferrer">llmux Cloud</a>
            </div>
            <div className="footer-col">
              <h4>Develop</h4>
              <a href={GH} target="_blank" rel="noreferrer">GitHub</a>
              <Link to="/docs/quickstart">Quickstart</Link>
              <Link to="/docs/providers">Providers</Link>
            </div>
            <div className="footer-col">
              <h4>Resources</h4>
              <Link to="/docs/routing">Routing</Link>
              <Link to="/docs/pricing">Pricing</Link>
              <a href={`${GH}/blob/main/LICENSE`} target="_blank" rel="noreferrer">MIT License</a>
            </div>
          </div>
        </div>
        <div className="wrap footer-base">
          <span>© {new Date().getFullYear()} llmux · MIT licensed</span>
          <span className="footer-mono">$ curl localhost:4000/v1/chat/completions</span>
        </div>
      </footer>
    </div>
  );
}
