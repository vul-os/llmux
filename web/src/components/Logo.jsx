// llmux brand mark — the multiplexer glyph (many input ports → one output)
// in white on a rounded teal tile. The tile is icon-only; the `llmux` wordmark
// (ll in text-primary, mux in accent) sits beside it, never on the tile.
// Mirrors web/public/llmux.svg. Tile fill is brand teal-400 (#2DD4BF).
export function Mark({ size = 32 }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 64 64" fill="none" aria-hidden="true">
      <rect x="0" y="0" width="64" height="64" rx="15" fill="#2DD4BF" />
      <g stroke="#fff" strokeWidth="2.6" strokeLinecap="round" strokeLinejoin="round" fill="none">
        <path d="M27 18 L46 29 L46 45 L27 56 Z" />
        <line x1="16" y1="20" x2="27" y2="20" />
        <line x1="16" y1="32" x2="27" y2="32" />
        <line x1="16" y1="44" x2="27" y2="44" />
        <line x1="46" y1="37" x2="56" y2="37" />
      </g>
      <g fill="#fff">
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
