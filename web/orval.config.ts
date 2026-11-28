// 接口代码从契约生成（Spec-first）：api/openapi.yaml 一份契约，服务端用 oapi-codegen、网页用 orval。
// 生成：npm run gen:api（CI 查生成的是不是最新的）。每个 tag 一个文件、纯函数（不是 hooks）；类型在 model/。
// 底层的请求都走 src/api/request.ts 的 apiRequest：带令牌、401 时刷新一次、Accept-Language。
import { defineConfig } from "orval";

export default defineConfig({
  haro: {
    input: "../api/openapi.yaml",
    output: {
      mode: "tags-split",
      target: "src/api/generated/haro.ts",
      schemas: "src/api/generated/model",
      client: "axios-functions",
      clean: true,
      override: {
        mutator: { path: "src/api/request.ts", name: "apiRequest" },
      },
    },
  },
});
