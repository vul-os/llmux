import { useEffect, useState } from "react";
import { api, getBase, setBase, getMasterKey, setMasterKey } from "../api.js";

function useAsync(fn, deps) {
  const [state, setState] = useState({ loading: true, data: null, error: null });
  useEffect(() => {
    let alive = true;
    setState({ loading: true, data: null, error: null });
    fn()
      .then((data) => alive && setState({ loading: false, data, error: null }))
      .catch((error) => alive && setState({ loading: false, data: null, error: String(error) }));
    return () => { alive = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return state;
}

const money = (n) => "$" + (Number(n) || 0).toFixed(4);

const TABS = ["usage", "keys", "models"];

export default function Dashboard() {
  const [tab, setTabState] = useState(() => {
    const h = (typeof location !== "undefined" ? location.hash.replace("#", "") : "");
    return TABS.includes(h) ? h : "usage";
  });
  const setTab = (t) => { setTabState(t); if (typeof history !== "undefined") history.replaceState(null, "", "#" + t); };
  const [refresh, setRefresh] = useState(0);
  const [base, setBaseInput] = useState(getBase());
  const [key, setKeyInput] = useState(getMasterKey());

  const apply = () => { setBase(base); setMasterKey(key); setRefresh((n) => n + 1); };

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
          <button className="btn primary" onClick={() => setRefresh((n) => n + 1)}>Refresh</button>
        </div>
      </div>

      <Health refresh={refresh} />

      <div className="tabs-row">
        {TABS.map((t) => (
          <button key={t} className={"tab" + (t === tab ? " active" : "")} onClick={() => setTab(t)}>{t}</button>
        ))}
      </div>

      {tab === "usage" && <Usage refresh={refresh} />}
      {tab === "keys" && <Keys refresh={refresh} />}
      {tab === "models" && <Models refresh={refresh} />}
    </main>
  );
}

function Health({ refresh }) {
  const { data, error } = useAsync(() => api.health(), [refresh]);
  if (error) return <div className="banner err">gateway unreachable — {error}</div>;
  if (!data) return null;
  const provs = (data.providers || []).map((p) => (typeof p === "string" ? p : `${p.name} · ${p.stability}`));
  return <div className="banner ok">● gateway online — providers: {provs.join(", ") || "none configured"}</div>;
}

function Usage({ refresh }) {
  const { loading, data, error } = useAsync(() => api.usage(), [refresh]);
  if (loading) return <p className="muted">loading…</p>;
  if (error) return <p className="err">{error}</p>;
  const total = data.total || {};
  const byModel = data.by_model || {};
  return (
    <div>
      <div className="cards">
        <Card label="Requests" value={total.requests || 0} />
        <Card label="Total tokens" value={(total.total_tokens || 0).toLocaleString()} />
        <Card label="Cost" value={money(total.cost_usd)} />
      </div>
      <Table cols={["model", "requests", "tokens", "cost"]}
        rows={Object.entries(byModel).map(([m, a]) => [m, a.requests, (a.total_tokens || 0).toLocaleString(), money(a.cost_usd)])} />
    </div>
  );
}

function Keys({ refresh }) {
  const { loading, data, error } = useAsync(() => api.keys(), [refresh]);
  if (loading) return <p className="muted">loading…</p>;
  if (error) return <p className="err">{error} — enter a master key above.</p>;
  return (
    <Table cols={["name", "key", "budget", "spend", "rpm"]}
      rows={(data.keys || []).map((k) => [k.name, k.key, money(k.budget_usd), money(k.spend_usd), k.rpm || "∞"])} />
  );
}

function Models({ refresh }) {
  const { loading, data, error } = useAsync(() => api.models(), [refresh]);
  if (loading) return <p className="muted">loading…</p>;
  if (error) return <p className="err">{error}</p>;
  const models = (data.data || []).slice(0, 500);
  return (
    <div>
      <p className="muted" style={{ fontSize: 13 }}>{(data.data || []).length} models in catalog</p>
      <Table cols={["id", "in $/Mtok", "out $/Mtok", "context"]}
        rows={models.map((m) => [m.id, m.input_price_per_mtok ?? "—", m.output_price_per_mtok ?? "—", (m.context_window || "—").toLocaleString?.() || "—"])} />
    </div>
  );
}

function Card({ label, value }) {
  return <div className="card"><div className="card-value">{value}</div><div className="card-label">{label}</div></div>;
}

function Table({ cols, rows }) {
  if (!rows.length) return <p className="muted">no data yet</p>;
  return (
    <table className="table">
      <thead><tr>{cols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
      <tbody>{rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j}>{c}</td>)}</tr>)}</tbody>
    </table>
  );
}
