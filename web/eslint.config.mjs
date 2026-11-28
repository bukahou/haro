// ESLint（flat config）：Next 16 没有 next lint，由 eslint 直接跑 Next 官方的规则集。
import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

export default defineConfig([
  ...nextVitals,
  ...nextTs,
  // 生成物、构建产物不查：接口代码由 orval 从契约生成（改契约后重新生成，⛔ 不手改）
  globalIgnores([".next/**", "out/**", "next-env.d.ts", "src/api/generated/**"]),
]);
