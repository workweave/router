// Shared chart palette using Weave HSL design tokens.
// Keep these in sync with globals.css if you change tokens.

export const CHART_COLORS = {
  brand: "hsl(12 82% 59%)",
  primary: "hsl(221 63% 55%)",
  success: "hsl(142 72% 29%)",
  danger: "hsl(0 84.2% 60.2%)",
  warning: "hsl(25 95% 53%)",
  info: "hsl(48 96% 53%)",
  foreground: "hsl(0 0% 3.9%)",
  muted: "hsl(0 0% 45.1%)",
  border: "hsl(0 0% 94.8%)",
  borderDarker: "hsl(0 0% 90%)",
} as const;

export const TOOLTIP_STYLE: React.CSSProperties = {
  fontSize: 12,
  border: "1px solid hsl(0 0% 90%)",
  borderRadius: "6px",
  padding: "8px 10px",
  background: "hsl(0 0% 100%)",
  boxShadow: "0 2px 8px rgba(0,0,0,0.06)",
};

export const AXIS_TICK = {
  fontSize: 11,
  fill: CHART_COLORS.muted,
} as const;

// Categorical palette for model mix, providers, etc.
export const SERIES_PALETTE = [
  "hsl(221 63% 55%)", // primary blue
  "hsl(12 82% 59%)", // brand orange
  "hsl(142 72% 29%)", // success green
  "hsl(280 70% 55%)", // purple
  "hsl(48 96% 53%)", // info yellow
  "hsl(200 80% 50%)", // cyan
  "hsl(340 70% 55%)", // pink
];
