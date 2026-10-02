import type { NextConfig } from "next"

// Where the Go scanner API lives. Rewrites are resolved at build time, so docker builds pass it
// as a build argument (default matches the compose service name).
const scannerUrl = process.env.SCANNER_URL ?? "http://localhost:8080"

const nextConfig: NextConfig = {
  output: "standalone",
  // Compression buffers small chunks, which would stall the live log stream (SSE).
  compress: false,
  async rewrites() {
    return [{ source: "/api/:path*", destination: `${scannerUrl}/api/:path*` }]
  },
}

export default nextConfig
