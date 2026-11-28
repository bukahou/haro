// App 接口的根地址：构建时由 NEXT_PUBLIC_API_URL 写进页面（next.config.ts 保证一定有）。
export const apiBaseURL = (process.env.NEXT_PUBLIC_API_URL ?? "").replace(/\/+$/, "");

// 登录入口（浏览器导航过去，不是 XHR）：client=web 让服务端登录完跳回网页的 /auth/callback；有邀请码时带上。
export function loginURL(invite?: string | null): string {
  const query = new URLSearchParams({ client: "web" });
  if (invite) {
    query.set("invite", invite);
  }
  return `${apiBaseURL}/api/v1/auth/oidc/start?${query.toString()}`;
}
