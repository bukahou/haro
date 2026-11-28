import type { NextConfig } from "next";

// NEXT_PUBLIC_API_URL（App 接口的根地址）在构建时写进页面：跟部署有关，没有默认值。缺了 next build / next dev 直接失败，
// 不悄悄连一个错的地址。本地开发写在 .env.development.local（不提交）；镜像构建时用 --build-arg 传入。
if (!process.env.NEXT_PUBLIC_API_URL) {
  throw new Error("NEXT_PUBLIC_API_URL 没设：本地开发写进 .env.development.local，构建镜像要 --build-arg NEXT_PUBLIC_API_URL");
}

const nextConfig: NextConfig = {
  output: "standalone", // 镜像里只带运行要的文件（node server.js）
  poweredByHeader: false,
  agentRules: false, // next dev 不在仓里生成 AGENTS.md
};

export default nextConfig;
