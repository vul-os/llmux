import { useRef, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";

// Curated highlight languages only — keeps the bundle small (vs all ~190).
import bash from "highlight.js/lib/languages/bash";
import python from "highlight.js/lib/languages/python";
import javascript from "highlight.js/lib/languages/javascript";
import typescript from "highlight.js/lib/languages/typescript";
import json from "highlight.js/lib/languages/json";
import go from "highlight.js/lib/languages/go";

const HL = { languages: { bash, sh: bash, python, javascript, js: javascript, typescript, json, go } };

import quickstart from "../../docs/quickstart.md?raw";
import providers from "../../docs/providers.md?raw";
import routing from "../../docs/routing.md?raw";
import pricing from "../../docs/pricing.md?raw";
import dashboard from "../../docs/dashboard.md?raw";

const DOCS = [
  { slug: "quickstart", title: "Quickstart", body: quickstart },
  { slug: "providers", title: "Providers", body: providers },
  { slug: "routing", title: "Routing & reliability", body: routing },
  { slug: "pricing", title: "Pricing & cost", body: pricing },
  { slug: "dashboard", title: "Dashboard & ops", body: dashboard },
];

const slugify = (s) =>
  String(s).toLowerCase().replace(/[^\w]+/g, "-").replace(/^-|-$/g, "");

function textOf(node) {
  if (typeof node === "string") return node;
  if (Array.isArray(node)) return node.map(textOf).join("");
  if (node?.props?.children) return textOf(node.props.children);
  return "";
}

function Heading({ level, children }) {
  const Tag = `h${level}`;
  const id = slugify(textOf(children));
  return <Tag id={id} className="md-h"><a href={`#${id}`} className="anchor" aria-hidden>#</a>{children}</Tag>;
}

function Pre({ children }) {
  const ref = useRef(null);
  const [copied, setCopied] = useState(false);
  const copy = () => {
    const text = ref.current?.innerText || "";
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1400);
    });
  };
  return (
    <div className="codeblock">
      <button className="copy" onClick={copy} aria-label="Copy code">{copied ? "copied ✓" : "copy"}</button>
      <pre ref={ref}>{children}</pre>
    </div>
  );
}

const COMPONENTS = {
  h1: (p) => <Heading level={1} {...p} />,
  h2: (p) => <Heading level={2} {...p} />,
  h3: (p) => <Heading level={3} {...p} />,
  pre: Pre,
};

export default function Docs() {
  const { slug } = useParams();
  const navigate = useNavigate();
  const active = DOCS.find((d) => d.slug === slug) || DOCS[0];
  const idx = DOCS.indexOf(active);
  const go2 = (s) => { navigate(`/docs/${s}`); window.scrollTo(0, 0); };

  return (
    <main className="wrap docs-layout">
      <nav className="docs-side">
        <p className="grp">Documentation</p>
        {DOCS.map((d) => (
          <a key={d.slug} href={`/docs/${d.slug}`}
            className={d.slug === active.slug ? "active" : ""}
            onClick={(e) => { e.preventDefault(); go2(d.slug); }}>
            {d.title}
          </a>
        ))}
      </nav>
      <article className="markdown">
        <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[[rehypeHighlight, HL]]} components={COMPONENTS}>
          {active.body}
        </ReactMarkdown>
        <div className="doc-nav">
          {idx > 0 && (
            <a className="doc-nav-btn" href={`/docs/${DOCS[idx - 1].slug}`} onClick={(e) => { e.preventDefault(); go2(DOCS[idx - 1].slug); }}>
              ← {DOCS[idx - 1].title}
            </a>
          )}
          <span style={{ flex: 1 }} />
          {idx < DOCS.length - 1 && (
            <a className="doc-nav-btn" href={`/docs/${DOCS[idx + 1].slug}`} onClick={(e) => { e.preventDefault(); go2(DOCS[idx + 1].slug); }}>
              {DOCS[idx + 1].title} →
            </a>
          )}
        </div>
      </article>
    </main>
  );
}
