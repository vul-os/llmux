/** @type {import('tailwindcss').Config} */
// llmux brand theme — mirrors src/tokens.css (the single source of truth).
// Color utilities resolve to the CSS custom properties so they track the
// active theme (dark default / light counterpart) without duplicating values.
// Static brand ramps (teal / neutral) are also exposed for one-off use.
export default {
  content: ["./index.html", "./src/**/*.{js,jsx,ts,tsx}"],
  darkMode: ["selector", '[data-theme="dark"]'],
  // Preflight off: the app ships a complete hand-written design system in
  // styles.css; we layer Tailwind utilities on top without resetting it.
  corePlugins: { preflight: false },
  theme: {
    extend: {
      colors: {
        // brand teal ramp
        teal: {
          50: "#F0FDFA", 100: "#CCFBF1", 200: "#99F6E4", 300: "#5EEAD4",
          400: "#2DD4BF", 500: "#14B8A6", 600: "#0D9488", 700: "#0F766E",
          800: "#115E59", 900: "#134E4A",
        },
        // neutral ramp (slate-tinted)
        neutral: {
          50: "#F8FAFA", 100: "#F1F5F5", 200: "#E2E8E8", 300: "#CBD5D5",
          400: "#94A3A3", 500: "#64748B", 600: "#475569", 700: "#334155",
          800: "#1E293B", 900: "#0F172A", 950: "#0A0F1A",
        },
        // semantic, theme-aware (CSS vars from tokens.css)
        bg: "var(--bg)",
        surface: "var(--surface)",
        "surface-raised": "var(--surface-raised)",
        border: "var(--border)",
        "border-subtle": "var(--border-subtle)",
        "text-primary": "var(--text-primary)",
        "text-secondary": "var(--text-secondary)",
        "text-tertiary": "var(--text-tertiary)",
        accent: {
          DEFAULT: "var(--accent)",
          hover: "var(--accent-hover)",
          muted: "var(--accent-muted)",
        },
        "on-accent": "var(--on-accent)",
        success: "var(--success)",
        warning: "var(--warning)",
        error: "var(--error)",
        info: "var(--info)",
      },
      fontFamily: {
        sans: "var(--font-sans)",
        mono: "var(--font-mono)",
      },
      fontSize: {
        h1: ["28px", { lineHeight: "1.15", fontWeight: "500" }],
        h2: ["22px", { lineHeight: "1.2", fontWeight: "500" }],
        h3: ["18px", { lineHeight: "1.3", fontWeight: "500" }],
        body: ["14px", { lineHeight: "1.6" }],
        caption: ["12px", { lineHeight: "1.5" }],
        code: ["13px", { lineHeight: "1.7" }],
      },
      fontWeight: {
        normal: "400",
        medium: "500",
      },
      borderRadius: {
        sm: "6px",
        md: "8px",
        lg: "12px",
        tile: "23%",
      },
      borderWidth: {
        hairline: "1px",
      },
      boxShadow: {
        focus: "0 0 0 3px var(--focus-ring)",
      },
      ringColor: {
        focus: "var(--focus-ring)",
      },
      transitionTimingFunction: {
        "ease-out-brand": "cubic-bezier(0.16,1,0.3,1)",
      },
      transitionDuration: {
        brand: "150ms",
      },
    },
  },
  plugins: [],
};
