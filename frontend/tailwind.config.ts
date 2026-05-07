import type { Config } from "tailwindcss";

const config: Config = {
  darkMode: ["class"],
  content: [
    "./src/pages/**/*.{js,ts,jsx,tsx,mdx}",
    "./src/components/**/*.{js,ts,jsx,tsx,mdx}",
    "./src/app/**/*.{js,ts,jsx,tsx,mdx}",
  ],
  theme: {
    colors: {
      transparent: "transparent",
      accent: {
        DEFAULT: "hsl(var(--accent))",
        foreground: "hsl(var(--accent-foreground))",
      },
      background: "hsl(var(--background))",
      border: "hsl(var(--border))",
      "border-darker": "hsl(var(--border-darker))",
      brand: "hsl(var(--brand))",
      card: {
        DEFAULT: "hsl(var(--card))",
        foreground: "hsl(var(--card-foreground))",
      },
      danger: {
        DEFAULT: "hsl(var(--danger))",
        foreground: "hsl(var(--danger-foreground))",
      },
      foreground: "hsl(var(--foreground))",
      info: {
        DEFAULT: "hsl(var(--info))",
        foreground: "hsl(var(--info-foreground))",
      },
      input: "hsl(var(--input))",
      muted: {
        DEFAULT: "hsl(var(--muted))",
        foreground: "hsl(var(--muted-foreground))",
      },
      popover: {
        DEFAULT: "hsl(var(--popover))",
        foreground: "hsl(var(--popover-foreground))",
      },
      primary: {
        DEFAULT: "hsl(var(--primary))",
        foreground: "hsl(var(--primary-foreground))",
      },
      ring: "hsl(var(--ring))",
      success: {
        DEFAULT: "hsl(var(--success))",
        foreground: "hsl(var(--success-foreground))",
      },
      tooltip: {
        DEFAULT: "hsl(var(--tooltip))",
        foreground: "hsl(var(--tooltip-foreground))",
      },
      warning: {
        DEFAULT: "hsl(var(--warning))",
        foreground: "hsl(var(--warning-foreground))",
      },
    },
    extend: {
      fontFamily: {
        sans: ["var(--font-inter)", "Inter", "system-ui", "sans-serif"],
        display: ["var(--font-inter-display)", "var(--font-inter)", "Inter", "system-ui", "sans-serif"],
      },
      fontSize: {
        "2xs": ["0.75rem", "0.875rem"],
        xs: ["0.8125rem", "1rem"],
      },
      borderRadius: {
        lg: "0.5rem",
        md: "calc(0.5rem - 2px)",
        sm: "calc(0.5rem - 4px)",
      },
      boxShadow: {
        "fake-border": "inset 0 -1px 0 hsl(var(--border))",
      },
      keyframes: {
        "fade-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        "fade-in-up": {
          from: { opacity: "0", transform: "translateY(-8px)" },
          to: { opacity: "1", transform: "translateY(0)" },
        },
      },
      animation: {
        "fade-in": "fade-in 0.25s ease-out",
        "fade-in-up": "fade-in-up 0.3s cubic-bezier(0.25, 0.1, 0.25, 1) forwards",
      },
    },
  },
  plugins: [],
};

export default config;
