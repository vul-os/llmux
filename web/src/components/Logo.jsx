// llmux brand mark — the multiplexer glyph (many input ports → one output),
// a solid dark-ink body on a rounded teal tile (brand/logo.svg's "fill the
// mux body solid" treatment, not the earlier wireframe outline). The tile is
// icon-only; the `llmux` wordmark (ll in text-primary, mux in accent) sits
// beside it, never on the tile. Mirrors web/public/llmux.svg and
// web/public/favicon.svg. Tile fill is brand teal-400 (#2DD4BF); glyph ink
// is brand dark (#0A0F1A).
export function Mark({ size = 32 }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 64 64" fill="none" aria-hidden="true">
      <rect x="0" y="0" width="64" height="64" rx="15" fill="#2DD4BF" />
      <g stroke="#0A0F1A" strokeWidth="2.6" strokeLinecap="round" strokeLinejoin="round" fill="none">
        <line x1="16" y1="20" x2="27" y2="20" />
        <line x1="16" y1="32" x2="27" y2="32" />
        <line x1="16" y1="44" x2="27" y2="44" />
        <line x1="46" y1="37" x2="56" y2="37" />
      </g>
      <path d="M27 18 L46 29 L46 45 L27 56 Z" fill="#0A0F1A" />
      <g fill="#0A0F1A">
        <circle cx="13" cy="20" r="2.2" />
        <circle cx="13" cy="32" r="2.2" />
        <circle cx="13" cy="44" r="2.2" />
        <circle cx="56" cy="37" r="2.2" />
      </g>
    </svg>
  );
}

export function Logo() {
  return (
    <span className="logo">
      <span className="logo-mark"><Mark size={32} /></span>
      <span className="word">ll<span className="x">mux</span></span>
    </span>
  );
}
