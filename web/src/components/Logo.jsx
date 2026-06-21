// llmux mark: a multiplexer — N inputs converge into the mux node, one channel out.
export function Mark({ size = 30 }) {
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 32 32" aria-hidden="true">
      <g stroke="var(--signal)" strokeWidth="1.6" fill="none" strokeLinecap="round" opacity="0.9">
        <path d="M4 7 H13" />
        <path d="M4 12.5 H13" />
        <path d="M4 19.5 H13" />
        <path d="M4 25 H13" />
      </g>
      <path
        d="M13 5 L22 10.5 L22 21.5 L13 27 Z"
        fill="var(--ink-3)"
        stroke="var(--signal)"
        strokeWidth="1.6"
        strokeLinejoin="round"
      />
      <path d="M22 16 H28" stroke="var(--mint)" strokeWidth="1.9" fill="none" strokeLinecap="round" />
      <circle cx="28" cy="16" r="1.9" fill="var(--mint)" />
    </svg>
  );
}

export function Logo() {
  return (
    <span className="logo">
      <Mark />
      <span className="word">llmu<span className="x">x</span></span>
    </span>
  );
}
