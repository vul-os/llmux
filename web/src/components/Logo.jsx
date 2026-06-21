import { useId } from "react";

// llmux mark — a multiplexer: N input channels converge into the mux gate and
// leave as one. A signal pulse rides the output cable (pauses on reduced-motion).
export function Mark({ size = 28 }) {
  const id = useId().replace(/:/g, "");
  const a = `a-${id}`, c = `c-${id}`, g = `g-${id}`;
  return (
    <svg className="mark" width={size} height={size} viewBox="0 0 36 36" fill="none" aria-hidden="true">
      <defs>
        <linearGradient id={a} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="var(--signal-soft)" />
          <stop offset="1" stopColor="var(--signal)" />
        </linearGradient>
        <linearGradient id={c} x1="0" y1="0" x2="1" y2="0">
          <stop offset="0" stopColor="var(--signal)" />
          <stop offset="1" stopColor="var(--mint)" />
        </linearGradient>
        <filter id={g} x="-80%" y="-80%" width="260%" height="260%">
          <feGaussianBlur stdDeviation="1.1" result="b" />
          <feMerge><feMergeNode in="b" /><feMergeNode in="SourceGraphic" /></feMerge>
        </filter>
      </defs>

      {/* inputs converging into the gate */}
      <g stroke={`url(#${a})`} strokeWidth="1.9" fill="none" strokeLinecap="round">
        <path d="M4 7 C13 7 13 16.4 17 16.6" />
        <path d="M4 13 C11 13 14 17 17 17.2" />
        <path d="M4 23 C11 23 14 19 17 18.8" />
        <path d="M4 29 C13 29 13 19.6 17 19.4" />
      </g>
      <g fill={`url(#${a})`}>
        <circle cx="4" cy="7" r="1.8" /><circle cx="4" cy="13" r="1.8" />
        <circle cx="4" cy="23" r="1.8" /><circle cx="4" cy="29" r="1.8" />
      </g>

      {/* mux gate */}
      <path d="M17 8 L26 13 L26 23 L17 28 Z" fill="var(--ink-3)" stroke={`url(#${a})`} strokeWidth="1.9" strokeLinejoin="round" />

      {/* output channel + node + travelling pulse */}
      <path d="M26 18 H33" stroke={`url(#${c})`} strokeWidth="2" strokeLinecap="round" />
      <circle cx="33" cy="18" r="2.1" fill="var(--mint)" filter={`url(#${g})`} />
      <circle className="logo-pulse" r="1.5" fill="var(--mint-soft)">
        <animateMotion dur="2.2s" repeatCount="indefinite" path="M17.5 18 H33" keyPoints="0;0;1;1" keyTimes="0;0.35;0.7;1" calcMode="linear" />
        <animate attributeName="opacity" dur="2.2s" repeatCount="indefinite" values="0;0;1;1;0;0" keyTimes="0;0.34;0.4;0.66;0.72;1" />
      </circle>
    </svg>
  );
}

export function Logo() {
  return (
    <span className="logo">
      <span className="logo-mark"><Mark /></span>
      <span className="word">llmu<span className="x">x</span></span>
    </span>
  );
}
