// llmux brand mark — the multiplexer glyph (many input ports → one output),
// a solid dark-ink body on a rounded teal tile. Geometry copied verbatim
// (same viewBox, same path/line/circle coordinates) from brand/logo.svg's
// inner mark group — the tile is icon-only; the `llmux` wordmark (ll in
// text-primary, mux in accent) sits beside it, never on the tile. Mirrors
// web/public/llmux.svg and web/public/favicon.svg. Tile fill is brand
// teal-400 (#2DD4BF); glyph ink is brand dark (#0A0F1A).
export function Mark({ size = 32 }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 100 100" fill="none" aria-hidden="true">
      <rect x="2" y="2" width="96" height="96" rx="24" fill="#2DD4BF" />
      <g transform="translate(16,18) scale(0.9)">
        <g stroke="#0A0F1A" strokeWidth="4.6" strokeLinecap="round">
          <line x1="8" y1="14" x2="32" y2="14" />
          <line x1="8" y1="34" x2="32" y2="34" />
          <line x1="8" y1="54" x2="32" y2="54" />
          <line x1="56" y1="34" x2="74" y2="34" />
        </g>
        <path d="M30 6 L56 23 L56 45 L30 62 Z" fill="#0A0F1A" />
        <g fill="#0A0F1A">
          <circle cx="8" cy="14" r="4.2" />
          <circle cx="8" cy="34" r="4.2" />
          <circle cx="8" cy="54" r="4.2" />
          <circle cx="74" cy="34" r="4.2" />
        </g>
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
