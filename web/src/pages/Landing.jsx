import { useState } from "react";
import { Link } from "react-router-dom";
import { motion } from "framer-motion";

const up = {
  hidden: { opacity: 0, y: 18 },
  show: (i = 0) => ({ opacity: 1, y: 0, transition: { duration: 0.6, delay: i * 0.08, ease: [0.22, 1, 0.36, 1] } }),
};

const SNIPPETS = {
  python: [
    ['from', 'kw'], [' openai ', ''], ['import', 'kw'], [' OpenAI\n', ''],
    ['client = OpenAI(base_url=', ''], ['"http://localhost:4000/v1"', 'st'], [', api_key=', ''], ['"sk-…"', 'st'], [')\n', ''],
    ['client.chat.completions.create(\n', ''],
    ['    model=', ''], ['"anthropic/claude-3-5-sonnet"', 'st'], [',  ', ''], ['# any provider, one client\n', 'cm'],
    ['    messages=[{', ''], ['"role"', 'st'], [': ', ''], ['"user"', 'st'], [', ', ''], ['"content"', 'st'], [': ', ''], ['"hi"', 'st'], ['}],\n)', ''],
  ],
  node: [
    ['import', 'kw'], [' OpenAI ', ''], ['from', 'kw'], [' ', ''], ['"openai"', 'st'], [';\n', ''],
    ['const client = ', ''], ['new', 'kw'], [' OpenAI({ baseURL: ', ''], ['"http://localhost:4000/v1"', 'st'], [' });\n', ''],
    ['await', 'kw'], [' client.chat.completions.create({\n', ''],
    ['  model: ', ''], ['"gemini-1.5-pro"', 'st'], [',\n', ''],
    ['  messages: [{ role: ', ''], ['"user"', 'st'], [', content: ', ''], ['"hi"', 'st'], [' }],\n});', ''],
  ],
  go: [
    ['local, _ := llmux.Start(llmux.Options{})  ', ''], ['// in-process, no server\n', 'cm'],
    ['defer', 'kw'], [' local.Close()\n', ''],
    ['// point any OpenAI Go client at local.OpenAIBaseURL()', 'cm'],
  ],
  curl: [
    ['curl', 'kw'], [' localhost:4000/v1/chat/completions \\\n', ''],
    ['  -H ', ''], ['"content-type: application/json"', 'st'], [' \\\n', ''],
    ['  -d ', ''], ['\'{"model":"cheapest","messages":[…]}\'', 'st'],
  ],
};

const FEATURES = [
  ["Every language", "M4 7h6M4 12h6M4 17h6", "Speak the OpenAI HTTP API. Your existing SDK in any language just swaps base_url — no port to maintain."],
  ["Every provider", "M3 12h4l3-8 4 16 3-8h4", "OpenAI, Anthropic, Gemini, Cohere, Bedrock, DeepSeek, Groq, OpenRouter, Ollama — one adapter seam."],
  ["Smart routing", "M6 3v6a3 3 0 003 3h9m0 0-3-3m3 3-3 3", "Aliases, fallbacks, retries, and least-cost selection across providers — declared in config."],
  ["Live cost", "M12 2v20M7 6h7a3 3 0 010 6H8a3 3 0 000 6h8", "Route-aware pricing auto-synced from OpenRouter + LiteLLM. Cost in every response's usage block."],
  ["Governance", "M12 3l8 4v6c0 5-4 8-8 9-4-1-8-4-8-9V7z", "Virtual keys with budgets, rate limits, and model allow-lists. Spend persisted in Postgres."],
  ["Caching", "M4 7c0-2 4-3 8-3s8 1 8 3-4 3-8 3-8-1-8-3zm0 0v10c0 2 4 3 8 3s8-1 8-3V7", "Exact-match and semantic (embedding-similarity) response caching — in-memory or Redis."],
];

const PROVIDERS = ["OpenAI", "Anthropic", "Gemini", "Cohere", "Bedrock", "DeepSeek", "Groq", "Mistral", "Together", "Fireworks", "xAI", "OpenRouter", "Ollama", "vLLM", "Perplexity"];

function Code({ tokens }) {
  return (
    <pre><code>{tokens.map(([t, c], i) => <span key={i} className={c}>{t}</span>)}</code></pre>
  );
}

function RoutingDiagram() {
  const inputs = [
    { y: 34, label: "openai" },
    { y: 78, label: "anthropic" },
    { y: 122, label: "gemini" },
    { y: 166, label: "cohere" },
    { y: 210, label: "+ 100 more" },
  ];
  return (
    <svg viewBox="0 0 440 300" width="100%" role="img" aria-label="Providers routed through the llmux multiplexer">
      <defs>
        <filter id="glow" x="-50%" y="-50%" width="200%" height="200%">
          <feGaussianBlur stdDeviation="4" result="b" />
          <feMerge><feMergeNode in="b" /><feMergeNode in="SourceGraphic" /></feMerge>
        </filter>
      </defs>
      {inputs.map((n, i) => (
        <g key={i}>
          <circle cx="30" cy={n.y} r="3.5" fill="var(--signal)" />
          <text className="node-label" x="42" y={n.y + 4}>{n.label}</text>
          <path
            className="cable"
            d={`M96 ${n.y} C150 ${n.y} 170 150 215 150`}
            stroke="var(--signal)" strokeWidth="1.5" fill="none" opacity="0.7"
          />
        </g>
      ))}
      {/* mux node */}
      <path d="M215 112 L262 132 L262 168 L215 188 Z" fill="var(--ink-3)" stroke="var(--signal)" strokeWidth="2" strokeLinejoin="round" filter="url(#glow)" />
      <text x="238" y="154" textAnchor="middle" fontFamily="var(--font-mono)" fontSize="9" fill="var(--muted)" letterSpacing="1">MUX</text>
      {/* output */}
      <path className="cable" d="M262 150 H392" stroke="var(--mint)" strokeWidth="2" fill="none" />
      <circle cx="392" cy="150" r="4" fill="var(--mint)" filter="url(#glow)" />
      <text className="node-label" x="372" y="135" textAnchor="middle" fill="var(--mint-soft)">your app</text>
    </svg>
  );
}

export default function Landing() {
  const [tab, setTab] = useState("python");
  return (
    <main>
      {/* ---------- hero ---------- */}
      <section className="hero">
        <div className="wrap hero-grid">
          <div>
            <motion.p className="eyebrow" variants={up} custom={0} initial="hidden" animate="show">
              open source · MIT · the LLM multiplexer
            </motion.p>
            <motion.h1 variants={up} custom={1} initial="hidden" animate="show">
              Every model.<br />Every language.<br /><span className="out">One channel.</span>
            </motion.h1>
            <motion.p className="sub" variants={up} custom={2} initial="hidden" animate="show">
              llmux is a single Go binary that speaks the OpenAI API and routes to <b>any provider</b> behind it.
              Point your existing OpenAI client at it — in <b>any language</b> — and get routing, fallbacks,
              budgets, caching, and live cost for free.
            </motion.p>
            <motion.div className="hero-cta" variants={up} custom={3} initial="hidden" animate="show">
              <Link className="btn primary" to="/docs">Get started →</Link>
              <Link className="btn" to="/app">Open dashboard</Link>
            </motion.div>
            <motion.div className="hero-meta" variants={up} custom={4} initial="hidden" animate="show">
              <span><b>100+</b> providers</span>
              <span><b>~0ms</b> overhead</span>
              <span><b>1</b> static binary</span>
              <span><b>$0</b> self-hosted</span>
            </motion.div>
          </div>
          <motion.div className="diagram" initial={{ opacity: 0, scale: 0.96 }} animate={{ opacity: 1, scale: 1 }} transition={{ duration: 0.8, delay: 0.2, ease: [0.22, 1, 0.36, 1] }}>
            <RoutingDiagram />
          </motion.div>
        </div>
      </section>

      {/* ---------- provider marquee ---------- */}
      <div className="marquee">
        <div className="marquee-track">
          {[...PROVIDERS, ...PROVIDERS].map((p, i) => <span key={i}><b>{p}</b></span>)}
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
          <div className="features">
            {FEATURES.map(([title, path, body], i) => (
              <motion.div className="feature" key={title} variants={up} custom={i} initial="hidden" whileInView="show" viewport={{ once: true }}>
                <svg className="ic" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d={path} /></svg>
                <h3>{title}</h3>
                <p>{body}</p>
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
            <div className="shot-bar"><span className="dot r" /><span className="dot y" /><span className="dot g" /></div>
            <img src="shots/dashboard.jpg" alt="llmux dashboard — usage, keys, and model catalog" loading="lazy" />
          </motion.div>
        </div>
      </section>

      {/* ---------- stats / pricing ---------- */}
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
        <div className="wrap" style={{ textAlign: "center" }}>
          <motion.h2 variants={up} initial="hidden" whileInView="show" viewport={{ once: true }} style={{ fontSize: "clamp(30px,5vw,52px)" }}>
            Self-host free. Forever.
          </motion.h2>
          <motion.p className="lede" variants={up} initial="hidden" whileInView="show" viewport={{ once: true }} style={{ margin: "0 auto 32px" }}>
            One binary, your keys, your infra. Or let llmux Cloud run it — and undercut the aggregators.
          </motion.p>
          <motion.div variants={up} initial="hidden" whileInView="show" viewport={{ once: true }} style={{ display: "flex", gap: 14, justifyContent: "center", flexWrap: "wrap" }}>
            <Link className="btn primary" to="/docs">Read the docs →</Link>
            <a className="btn" href="https://github.com/llmux/llmux">Star on GitHub</a>
          </motion.div>
        </div>
      </section>
    </main>
  );
}
