import { useState } from "react";
import { Link } from "react-router-dom";
import { motion } from "framer-motion";

// Canonical repo — matches App.jsx's GH constant (github.com/vul-os/llmux).
const GH = "https://github.com/vul-os/llmux";

const up = {
  hidden: { opacity: 0, y: 20 },
  show: (i = 0) => ({ opacity: 1, y: 0, transition: { duration: 0.6, delay: i * 0.07, ease: [0.22, 1, 0.36, 1] } }),
};

const SNIPPETS = {
  python: [
    ['from', 'kw'], [' openai ', ''], ['import', 'kw'], [' OpenAI\n', ''],
    ['client = OpenAI(base_url=', ''], ['"http://localhost:4000/v1"', 'st'], [', api_key=', ''], ['"sk-…"', 'st'], [')\n\n', ''],
    ['client.chat.completions.create(\n', ''],
    ['    model=', ''], ['"anthropic/claude-3-5-sonnet"', 'st'], [',  ', ''], ['# any provider, one client\n', 'cm'],
    ['    messages=[{', ''], ['"role"', 'st'], [': ', ''], ['"user"', 'st'], [', ', ''], ['"content"', 'st'], [': ', ''], ['"hi"', 'st'], ['}],\n)', ''],
  ],
  node: [
    ['import', 'kw'], [' OpenAI ', ''], ['from', 'kw'], [' ', ''], ['"openai"', 'st'], [';\n', ''],
    ['const client = ', ''], ['new', 'kw'], [' OpenAI({ baseURL: ', ''], ['"http://localhost:4000/v1"', 'st'], [' });\n\n', ''],
    ['await', 'kw'], [' client.chat.completions.create({\n', ''],
    ['  model: ', ''], ['"gemini-1.5-pro"', 'st'], [',\n', ''],
    ['  messages: [{ role: ', ''], ['"user"', 'st'], [', content: ', ''], ['"hi"', 'st'], [' }],\n});', ''],
  ],
  go: [
    ['local, _ := llmux.Start(llmux.Options{})  ', ''], ['// in-process, no server\n', 'cm'],
    ['defer', 'kw'], [' local.Close()\n\n', ''],
    ['// point any OpenAI Go client at local.OpenAIBaseURL()\n', 'cm'],
    ['// every language: just swap base_url', 'cm'],
  ],
  curl: [
    ['curl', 'kw'], [' localhost:4000/v1/chat/completions \\\n', ''],
    ['  -H ', ''], ['"content-type: application/json"', 'st'], [' \\\n', ''],
    ['  -d ', ''], ['\'{"model":"cheapest","messages":[…]}\'\n', 'st'],
    ['# routes to the cheapest live-priced model', 'cm'],
  ],
};

const FEATURES = [
  ["Every language", "M4 7h6M4 12h6M4 17h6", "Speak the OpenAI HTTP API. Your existing SDK — Python, Node, Go, Ruby, Rust — just swaps base_url. Nothing else to learn."],
  ["Every provider", "M3 12h4l3-8 4 16 3-8h4", "OpenAI, Anthropic, Gemini, Cohere, Bedrock, Azure, DeepSeek, Groq, OpenRouter, Ollama — behind one adapter seam."],
  ["Smart routing", "M6 3v6a3 3 0 003 3h9m0 0-3-3m3 3-3 3", "Aliases, prefix wildcards, fallback chains, retries, and least-cost selection across providers — declared in config."],
  ["Live cost", "M12 2v20M7 6h7a3 3 0 010 6H8a3 3 0 000 6h8", "Route-aware pricing auto-synced from OpenRouter + LiteLLM. Real cost in every response's usage block, billed in micro-dollars."],
  ["Governance", "M12 3l8 4v6c0 5-4 8-8 9-4-1-8-4-8-9V7z", "Virtual keys with budgets, rate limits, and model allow-lists. Spend persisted in Postgres, limits in Redis."],
  ["Caching", "M4 7c0-2 4-3 8-3s8 1 8 3-4 3-8 3-8-1-8-3zm0 0v10c0 2 4 3 8 3s8-1 8-3V7", "Exact-match and semantic (embedding-similarity) response caching — in-memory or shared via Redis."],
];

const PROVIDERS = ["OpenAI", "Anthropic", "Gemini", "Cohere", "Bedrock", "Azure", "DeepSeek", "Groq", "Mistral", "Together", "Fireworks", "xAI", "OpenRouter", "Ollama", "vLLM", "Perplexity"];

const STEPS = [
  ["Point", "Set your OpenAI client's", "base_url", "to llmux. One line — the rest of your code is untouched."],
  ["Route", "The", "model", "string picks the provider. Add fallbacks, retries, or least-cost routing in config."],
  ["Observe", "Every response carries live", "usage + cost", ". Track spend per key in the built-in dashboard."],
];

const COMPARE = [
  ["Single binary, no runtime", ["yes", "✓ one Go binary"], ["no", "Python app"], ["no", "hosted SaaS"]],
  ["Drop-in OpenAI API, any language", ["yes", "✓"], ["yes", "✓ proxy"], ["yes", "✓ API"]],
  ["Self-host, bring your own keys", ["yes", "✓"], ["yes", "✓"], ["no", "—"]],
  ["Routing + fallback + least-cost", ["yes", "✓"], ["yes", "✓"], ["partial", "auto only"]],
  ["Exact + semantic caching", ["yes", "✓"], ["yes", "✓"], ["no", "—"]],
  ["Live cost in every response", ["yes", "✓"], ["partial", "partial"], ["yes", "✓"]],
  ["Provider breadth", ["partial", "6 + passthrough"], ["yes", "100+"], ["yes", "300+"]],
  ["Battle-tested maturity", ["partial", "new"], ["yes", "✓"], ["yes", "✓"]],
];

function Code({ tokens }) {
  return <pre><code>{tokens.map(([t, c], i) => <span key={i} className={c}>{t}</span>)}</code></pre>;
}

function RoutingDiagram() {
  const inputs = [
    { y: 30, label: "openai" },
    { y: 72, label: "anthropic" },
    { y: 114, label: "gemini" },
    { y: 156, label: "bedrock" },
    { y: 198, label: "+ 100 more" },
  ];
  return (
    <svg viewBox="0 0 440 260" width="100%" role="img" aria-label="Providers routed through the llmux multiplexer">
      <defs>
        <filter id="dg" x="-60%" y="-60%" width="220%" height="220%">
          <feGaussianBlur stdDeviation="3.4" result="b" /><feMerge><feMergeNode in="b" /><feMergeNode in="SourceGraphic" /></feMerge>
        </filter>
        <linearGradient id="dch" x1="0" y1="0" x2="1" y2="0">
          <stop offset="0" stopColor="var(--signal)" /><stop offset="1" stopColor="var(--mint)" />
        </linearGradient>
      </defs>
      {inputs.map((n, i) => {
        const d = `M96 ${n.y} C150 ${n.y} 168 130 214 130`;
        return (
          <g key={i}>
            <circle cx="30" cy={n.y} r="3.2" fill="var(--signal)" />
            <text className="node-label" x="42" y={n.y + 4}>{n.label}</text>
            <path className="cable" d={d} stroke="var(--signal)" strokeWidth="1.4" fill="none" opacity="0.6" />
            {i % 2 === 0 && (
              <circle r="2" fill="var(--signal-soft)" className="pulse-dot">
                <animateMotion dur="2.6s" begin={`${i * 0.4}s`} repeatCount="indefinite" path={d} />
              </circle>
            )}
          </g>
        );
      })}
      <path d="M214 92 L262 114 L262 146 L214 168 Z" fill="var(--ink-3)" stroke="var(--signal)" strokeWidth="2" strokeLinejoin="round" filter="url(#dg)" />
      <text x="238" y="134" textAnchor="middle" fontFamily="var(--font-mono)" fontSize="9" fill="var(--muted)" letterSpacing="1">MUX</text>
      <path className="cable" d="M262 130 H398" stroke="url(#dch)" strokeWidth="2.2" fill="none" />
      <circle r="2.4" fill="var(--mint-soft)" className="pulse-dot">
        <animateMotion dur="1.6s" repeatCount="indefinite" path="M262 130 H398" />
      </circle>
      <circle cx="398" cy="130" r="4.2" fill="var(--mint)" filter="url(#dg)" />
      <text className="node-label" x="398" y="115" textAnchor="middle" fill="var(--mint-soft)">your app</text>
    </svg>
  );
}

const onTilt = (e) => {
  const r = e.currentTarget.getBoundingClientRect();
  e.currentTarget.style.setProperty("--mx", `${((e.clientX - r.left) / r.width) * 100}%`);
};

export default function Landing() {
  const [tab, setTab] = useState("python");
  return (
    <main>
      {/* ---------- hero ---------- */}
      <section className="hero">
        <div className="wrap hero-grid">
          <div>
            <motion.div className="hero-pill" variants={up} custom={0} initial="hidden" animate="show">
              <span className="pip" /> part of <b>Vulos</b> · open source · the LLM gateway
            </motion.div>
            <motion.h1 variants={up} custom={1} initial="hidden" animate="show">
              Every model.<br />Every language.<br /><span className="out grad-text">One channel.</span>
            </motion.h1>
            <motion.p className="sub" variants={up} custom={2} initial="hidden" animate="show">
              A single Go binary that speaks the OpenAI API and routes to <b>any provider</b> behind it.
              Point your existing client at it — in <b>any language</b> — and get routing, fallbacks,
              budgets, caching, and live cost for free.
            </motion.p>
            <motion.div className="hero-cta" variants={up} custom={3} initial="hidden" animate="show">
              <Link className="btn primary big" to="/docs">Get started <span aria-hidden="true">→</span></Link>
              <a className="btn big" href={GH} target="_blank" rel="noreferrer">Star on GitHub</a>
            </motion.div>
            <motion.div className="hero-meta" variants={up} custom={4} initial="hidden" animate="show">
              <span className="chip"><b>100+</b> providers</span>
              <span className="chip"><b>~9MB</b> binary</span>
              <span className="chip"><b>0</b> dependencies</span>
              <span className="chip"><b>$0</b> self-hosted</span>
            </motion.div>
          </div>

          <motion.div className="instrument" initial={{ opacity: 0, y: 26, scale: 0.97 }} animate={{ opacity: 1, y: 0, scale: 1 }} transition={{ duration: 0.85, delay: 0.2, ease: [0.22, 1, 0.36, 1] }}>
            <div className="instrument-head">
              <span className="label">Multiplexer</span>
              <span className="live"><span className="pip" /> routing</span>
            </div>
            <div className="instrument-body"><RoutingDiagram /></div>
            <div className="instrument-foot">
              <span><b>6</b> adapters</span>
              <span><b>688+</b> priced models</span>
              <span><b>1</b> OpenAI API</span>
            </div>
          </motion.div>
        </div>
      </section>

      {/* ---------- provider marquee ---------- */}
      <div className="marquee">
        <div className="marquee-track">
          {[...PROVIDERS, ...PROVIDERS].map((p, i) => (
            <span className="pv" key={i}><span className="pd" />{p}</span>
          ))}
        </div>
      </div>

      {/* ---------- code ---------- */}
      <section className="section">
        <div className="wrap">
          <motion.p className="eyebrow" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>drop-in</motion.p>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>Change one line. Keep your stack.</motion.h2>
          <motion.p className="lede" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            No SDK to learn. The OpenAI client you already use just points at llmux — and the model string picks the provider.
          </motion.p>
          <motion.div className="code" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            <div className="code-head">
              <span className="dot r" /><span className="dot y" /><span className="dot g" />
              <div className="code-tabs">
                {Object.keys(SNIPPETS).map((k) => (
                  <button key={k} className={"tab" + (k === tab ? " active" : "")} onClick={() => setTab(k)}>{k}</button>
                ))}
              </div>
            </div>
            <Code tokens={SNIPPETS[tab]} />
          </motion.div>
        </div>
      </section>

      {/* ---------- features ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <motion.p className="eyebrow" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>the gateway</motion.p>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>A control plane for every call.</motion.h2>
          <div className="bento">
            {FEATURES.map(([title, path, body], i) => (
              <motion.div className="feature" key={title} onMouseMove={onTilt} variants={up} custom={i} initial="hidden" whileInView="show" viewport={{ once: true }}>
                <span className="ic"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"><path d={path} /></svg></span>
                <h3>{title}</h3>
                <p>{body}</p>
              </motion.div>
            ))}
          </div>
        </div>
      </section>

      {/* ---------- how it works ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <motion.p className="eyebrow" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>how it works</motion.p>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>Three moves to one channel.</motion.h2>
          <div className="flow">
            {STEPS.map(([n, pre, code, post], i) => (
              <motion.div className="flow-step" key={n} variants={up} custom={i} initial="hidden" whileInView="show" viewport={{ once: true }}>
                <span className="step-n">{String(i + 1).padStart(2, "0")}</span>
                <h3>{n}</h3>
                <p>{pre} <code>{code}</code>{post}</p>
                <svg className="conn" viewBox="0 0 18 24" fill="none" stroke="currentColor" strokeWidth="1.5"><path d="M2 12h12m0 0-4-4m4 4-4 4" strokeLinecap="round" strokeLinejoin="round" /></svg>
              </motion.div>
            ))}
          </div>
        </div>
      </section>

      {/* ---------- dashboard preview ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <motion.p className="eyebrow" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>built in</motion.p>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>See spend, keys, and models — live.</motion.h2>
          <motion.p className="lede" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            A dashboard ships inside the binary at <code style={{ fontFamily: "var(--font-mono)", color: "var(--signal-soft)" }}>/ui</code> — usage by model, virtual-key budgets, and the live price catalog. No extra service to run.
          </motion.p>
          <motion.div className="shot" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            <div className="shot-bar"><span className="dot r" /><span className="dot y" /><span className="dot g" /><span className="addr">localhost:4000/ui</span></div>
            <img src="shots/dashboard.jpg" alt="llmux dashboard — usage, keys, and model catalog" loading="lazy" />
          </motion.div>
        </div>
      </section>

      {/* ---------- comparison ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <motion.p className="eyebrow" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>honestly</motion.p>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>Where llmux fits.</motion.h2>
          <motion.p className="lede" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            Best-in-class for self-hosted, single-binary deployments. Still younger than the incumbents on breadth and battle-testing — and we say so.
          </motion.p>
          <motion.div className="compare" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            <table>
              <thead><tr><th>Capability</th><th className="us">llmux</th><th>LiteLLM</th><th>OpenRouter</th></tr></thead>
              <tbody>
                {COMPARE.map(([cap, a, b, c]) => (
                  <tr key={cap}>
                    <td>{cap}</td>
                    <td className={"us " + a[0]}>{a[1]}</td>
                    <td className={b[0]}>{b[1]}</td>
                    <td className={c[0]}>{c[1]}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </motion.div>
        </div>
      </section>

      {/* ---------- stats ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <div className="stats">
            <div className="stat"><div className="n">100<span className="u">+</span></div><div className="l">providers, one API</div></div>
            <div className="stat"><div className="n">688<span className="u">+</span></div><div className="l">live-priced models</div></div>
            <div className="stat"><div className="n">~9<span className="u">MB</span></div><div className="l">single static binary</div></div>
            <div className="stat"><div className="n">100<span className="u">%</span></div><div className="l">open source, MIT</div></div>
          </div>
        </div>
      </section>

      {/* ---------- cta ---------- */}
      <section className="section" style={{ paddingTop: 0 }}>
        <div className="wrap">
          <motion.div className="cta-panel" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }}>
            <h2>Self-host free. Forever.</h2>
            <p className="lede">One binary, your keys, your infra. Or let llmux Cloud run it — and undercut the aggregators.</p>
            <div className="hero-cta">
              <Link className="btn primary big" to="/docs">Read the docs <span aria-hidden="true">→</span></Link>
              <a className="btn big" href={GH} target="_blank" rel="noreferrer">Star on GitHub</a>
            </div>
          </motion.div>
        </div>
      </section>
    </main>
  );
}
