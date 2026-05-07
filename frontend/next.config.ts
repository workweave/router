import type { NextConfig } from "next";

const isDev = process.env.NODE_ENV === "development";
const ROUTER_DEV_TARGET = process.env.ROUTER_DEV_TARGET ?? "http://localhost:8080";

const nextConfig: NextConfig = {
  // Static export for production; dev server uses local .next so the runtime
  // can resolve next/* modules from frontend/node_modules.
  ...(isDev
    ? {
        async rewrites() {
          // Proxy admin API calls to the Go router so the dashboard can
          // hit /admin/v1/* without CORS or absolute URLs.
          return [
            {
              source: "/admin/:path*",
              destination: `${ROUTER_DEV_TARGET}/admin/:path*`,
              basePath: false,
            },
          ];
        },
        // Bare-root convenience redirect so visiting localhost:3000
        // lands on the dashboard. In production the Go server does
        // this; in `next dev` requests hit Next directly and never
        // reach the Go backend, so we replicate it here.
        async redirects() {
          return [
            {
              source: "/",
              destination: "/ui/",
              basePath: false,
              permanent: false,
            },
          ];
        },
      }
    : {
        output: "export",
        distDir: "../assets/ui",
      }),
  basePath: "/ui",
  devIndicators: false,
  images: { unoptimized: true },
  trailingSlash: true,
};

export default nextConfig;
