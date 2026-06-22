import { useEffect, useRef, useState } from "react";
import { api, getBase, setBase, getMasterKey, setMasterKey } from "../api.js";

// Async hook with manual + interval refresh. `refresh` bumps re-run the fetch;
// `intervalMs` (when set) polls in the background without flashing the loader,
// and exposes `updatedAt` so the UI can show a subtle freshness indicator.
function useAsync(fn, deps, intervalMs) {
  const [state, setState] = useState({ loading: true, data: null, error: null, updatedAt: null });
  const fnRef = useRef(fn);
  fnRef.current = fn;

  useEffect(() => {
    let alive = true;
    const run = (background) => {
      if (!background) setState((s) => ({ ...s, loading: true, error: null }));
      fnRef.current()
        .then((data) => alive && setState({ loading: false, data, error: null, updatedAt: Date.now() }))
        .catch((error) => alive && setState((s) => ({ loading: false, data: background ? s.data : null, error: String(error), updatedAt: s.updatedAt })));
    };
    run(false);
    let id;
    if (intervalMs) id = setInterval(() => run(true), intervalMs);
    return () => { alive = false; if (id) clearInterval(id); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, intervalMs]);

  return state;
}

const money = (n) => {
  const v = Number(n) || 0;
  // Keep small per-request costs readable, but don't drown big totals in zeros.
  const digits = v !== 0 && Math.abs(v) < 1 ? 4 : 2;
  return "$" + v.toLocaleString(undefined, { minimumFractionDigits: digits, maximumFractionDigits: digits });
};
const fmtInt = (n) => (Number(n) || 0).toLocaleString();
// Compact token counts (1.2M, 48.5k) for cards; full count available on hover.
const fmtTokens = (n) => {
  const v = Number(n) || 0;
  if (v >= 1e9) return (v / 1e9).toFixed(2) + "B";
  if (v >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (v >= 1e4) return (v / 1e3).toFixed(1) + "k";
  return v.toLocaleString();
};
const fmtAgo = (ts) => {
  if (!ts) return null;
  const s = Math.max(0, Math.round((Date.now() - ts) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return s + "s ago";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m ago";
  return Math.floor(m / 60) + "h ago";
};

const TABS = ["usage", "keys", "models"];
const POLL_MS = 15000;

export default function Dashboard() {
  const [tab, setTabState] = useState(() => {
    const h = (typeof location !== "undefined" ? location.hash.replace("#", "") : "");
    return TABS.includes(h) ? h : "usage";
  });
  const setTab = (t) => { setTabState(t); if (typeof history !== "undefined") history.replaceState(null, "", "#" + t); };
  const [refresh, setRefresh] = useState(0);
  const [auto, setAuto] = useState(true);
  const [base, setBaseInput] = useState(getBase());
  const [key, setKeyInput] = useState(getMasterKey());

  const apply = () => { setBase(base); setMasterKey(key); setRefresh((n) => n + 1); };
  const poll = auto ? POLL_MS : 0;

  return (
    <main className="wrap dash">
      <div className="dash-head">
        <div>
          <h1>Dashboard</h1>
          <p className="muted" style={{ margin: "4px 0 0", fontSize: 14 }}>Usage, keys, and the live model catalog.</p>
        </div>
        <div className="settings">
          <input placeholder="gateway URL (blank = same origin)" value={base} onChange={(e) => setBaseInput(e.target.value)} />
          <input placeholder="master key" type="password" value={key} onChange={(e) => setKeyInput(e.target.value)} />
          <button className="btn" onClick={apply}>Apply</button>
          <button
            className={"btn auto-toggle" + (auto ? " on" : "")}
            onClick={() => setAuto((a) => !a)}
            aria-pressed={auto}
            title={auto ? "Auto-refresh on — click to pause" : "Auto-refresh paused — click to resume"}
          >
            <span className={"pip" + (auto ? " live" : "")} aria-hidden="true" />
            {auto ? "Live" : "Paused"}
          </button>
          <button className="btn primary" onClick={() => setRefresh((n) => n + 1)}>Refresh</button>
        </div>
      </div>

      <Health refresh={refresh} poll={poll} />

      <div className="tabs-row" role="tablist">
        {TABS.map((t) => (
          <button key={t} role="tab" aria-selected={t === tab} className={"tab" + (t === tab ? " active" : "")} onClick={() => setTab(t)}>{t}</button>
        ))}
      </div>

      {tab === "usage" && <Usage refresh={refresh} poll={poll} />}
      {tab === "keys" && <Keys refresh={refresh} poll={poll} />}
      {tab === "models" && <Models refresh={refresh} poll={poll} />}
    </main>
  );
}

function Health({ refresh, poll }) {
  const { data, error, loading } = useAsync(() => api.health(), [refresh], poll);
  if (loading && !data) return <div className="banner skel-banner" aria-busy="true" />;
  if (error) return <div className="banner err">gateway unreachable — {error}</div>;
  if (!data) return null;
  const provs = (data.providers || []).map((p) => (typeof p === "string" ? p : `${p.name} · ${p.stability}`));
  return <div className="banner ok"><span className="pip live" aria-hidden="true" /> gateway online — providers: {provs.join(", ") || "none configured"}</div>;
}

function Freshness({ updatedAt }) {
  // Re-render once a second so the relative timestamp stays current.
  const [, tick] = useState(0);
  useEffect(() => {
    if (!updatedAt) return;
    const id = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [updatedAt]);
  const ago = fmtAgo(updatedAt);
  if (!ago) return null;
  return <span className="freshness">updated {ago}</span>;
}

function Usage({ refresh, poll }) {
  const { loading, data, error, updatedAt } = useAsync(() => api.usage(), [refresh], poll);
  if (loading && !data) return <UsageSkeleton />;
  if (error) return <ErrorState error={error} />;
  const total = data.total || {};
  const byModel = data.by_model || {};
  const rows = Object.entries(byModel);
  return (
    <div>
      <div className="section-head">
        <span className="section-eyebrow">Spend &amp; volume</span>
        <Freshness updatedAt={updatedAt} />
      </div>
      <div className="cards">
        <Card label="Requests" value={fmtInt(total.requests)} />
        <Card label="Total tokens" value={fmtTokens(total.total_tokens)} title={fmtInt(total.total_tokens) + " tokens"} />
        <Card label="Cost" value={money(total.cost_usd)} accent />
      </div>
      {rows.length === 0 ? (
        <EmptyState
          title="No usage yet"
          hint="Once requests flow through the gateway, per-model spend and token counts will appear here."
        />
      ) : (
        <Table
          cols={["model", "requests", "tokens", "cost"]}
          align={["left", "right", "right", "right"]}
          rows={rows.map(([m, a]) => [
            <span className="cell-id" title={m}>{m}</span>,
            fmtInt(a.requests),
            <span title={fmtInt(a.total_tokens) + " tokens"}>{fmtTokens(a.total_tokens)}</span>,
            money(a.cost_usd),
          ])}
        />
      )}
    </div>
  );
}

function Keys({ refresh, poll }) {
  const { loading, data, error, updatedAt } = useAsync(() => api.keys(), [refresh], poll);
  if (loading && !data) return <TableSkeleton cols={5} />;
  if (error) return <ErrorState error={error} hint="This view needs a master key — enter one above and hit Apply." />;
  const keys = data.keys || [];
  if (keys.length === 0) {
    return <EmptyState title="No API keys" hint="Issued keys, their budgets, and current spend will be listed here." />;
  }
  return (
    <div>
      <div className="section-head">
        <span className="section-eyebrow">{keys.length} key{keys.length === 1 ? "" : "s"}</span>
        <Freshness updatedAt={updatedAt} />
      </div>
      <Table
        cols={["name", "key", "budget", "spend", "rpm"]}
        align={["left", "left", "right", "right", "right"]}
        rows={keys.map((k) => [
          k.name || "—",
          <code className="key-token" title={k.key}>{k.key}</code>,
          money(k.budget_usd),
          <Spend spend={k.spend_usd} budget={k.budget_usd} />,
          k.rpm || "∞",
        ])}
      />
    </div>
  );
}

// Spend cell with a tiny budget bar; turns persimmon as it nears the cap.
function Spend({ spend, budget }) {
  const s = Number(spend) || 0;
  const b = Number(budget) || 0;
  const pct = b > 0 ? Math.min(100, (s / b) * 100) : 0;
  const near = pct >= 90;
  return (
    <span className="spend-cell">
      {money(s)}
      {b > 0 && (
        <span className="spend-bar" aria-hidden="true">
          <span className={"spend-fill" + (near ? " near" : "")} style={{ width: pct + "%" }} />
        </span>
      )}
    </span>
  );
}

function Models({ refresh, poll }) {
  const { loading, data, error, updatedAt } = useAsync(() => api.models(), [refresh], poll);
  const [q, setQ] = useState("");
  if (loading && !data) return <TableSkeleton cols={4} />;
  if (error) return <ErrorState error={error} />;
  const all = data.data || [];
  const filtered = q ? all.filter((m) => String(m.id).toLowerCase().includes(q.toLowerCase())) : all;
  const models = filtered.slice(0, 500);
  return (
    <div>
      <div className="section-head">
        <span className="section-eyebrow">{all.length} model{all.length === 1 ? "" : "s"} in catalog</span>
        <div className="section-head-right">
          <input className="filter-input" placeholder="filter models…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter models" />
          <Freshness updatedAt={updatedAt} />
        </div>
      </div>
      {models.length === 0 ? (
        <EmptyState
          title={q ? "No matching models" : "No models in catalog"}
          hint={q ? "Try a different search term." : "The gateway will populate this list from its configured providers."}
        />
      ) : (
        <Table
          cols={["id", "in $/Mtok", "out $/Mtok", "context"]}
          align={["left", "right", "right", "right"]}
          rows={models.map((m) => [
            <span className="cell-id" title={m.id}>{m.id}</span>,
            m.input_price_per_mtok ?? "—",
            m.output_price_per_mtok ?? "—",
            (m.context_window || "—").toLocaleString?.() || "—",
          ])}
        />
      )}
    </div>
  );
}

function Card({ label, value, title, accent }) {
  return (
    <div className={"card" + (accent ? " accent" : "")}>
      <div className="card-value" title={title}>{value}</div>
      <div className="card-label">{label}</div>
    </div>
  );
}

function Table({ cols, rows, align }) {
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>{cols.map((c, i) => <th key={c} style={align ? { textAlign: align[i] } : undefined}>{c}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>{r.map((c, j) => <td key={j} style={align ? { textAlign: align[j] } : undefined}>{c}</td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EmptyState({ title, hint }) {
  return (
    <div className="empty-state">
      <svg viewBox="0 0 24 24" width="28" height="28" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M3 7l9-4 9 4v10l-9 4-9-4V7Z" />
        <path d="M3 7l9 4 9-4M12 11v10" />
      </svg>
      <div className="empty-title">{title}</div>
      <div className="empty-hint">{hint}</div>
    </div>
  );
}

function ErrorState({ error, hint }) {
  return (
    <div className="error-state">
      <svg viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="9" />
        <path d="M12 7.5v5M12 16h.01" />
      </svg>
      <div className="error-title">Couldn’t load this view</div>
      <div className="error-detail">{error}</div>
      {hint && <div className="error-hint">{hint}</div>}
    </div>
  );
}

function UsageSkeleton() {
  return (
    <div aria-busy="true">
      <div className="cards">
        {[0, 1, 2].map((i) => (
          <div className="card" key={i}>
            <div className="skel skel-lg" />
            <div className="skel skel-sm" style={{ marginTop: 10 }} />
          </div>
        ))}
      </div>
      <TableSkeleton cols={4} />
    </div>
  );
}

function TableSkeleton({ cols = 4, rows = 5 }) {
  return (
    <div className="table-wrap" aria-busy="true">
      <table className="table skel-table">
        <thead>
          <tr>{Array.from({ length: cols }).map((_, i) => <th key={i}><span className="skel skel-sm" /></th>)}</tr>
        </thead>
        <tbody>
          {Array.from({ length: rows }).map((_, r) => (
            <tr key={r}>{Array.from({ length: cols }).map((_, c) => <td key={c}><span className="skel skel-row" /></td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
