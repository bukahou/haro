// 接口请求的唯一出口（orval 生成的函数都经 apiRequest）：带上访问令牌和界面语言；访问令牌过期（401）时用刷新令牌换一对新的、
// 重发一次；刷新也失败就登出、回登录页。
//
// ⚠️ 刷新令牌用一次就作废，再用旧的会被服务端当成被盗、整串作废（所有登录都掉）。所以刷新必须「整个浏览器同时只有一次」：
//   - 同一个页面里并发的 401 共用同一次刷新（refreshing）；
//   - 几个标签页之间用 Web Locks 排队，拿到锁后重新读一次令牌：等锁期间别的标签页可能已经换过了，⛔ 不能拿旧的刷新令牌再去换。
import axios, { AxiosError, type AxiosRequestConfig } from "axios";

import { useSession, type Tokens } from "@/auth/session";

import { apiBaseURL } from "./config";

const client = axios.create({ baseURL: apiBaseURL, timeout: 30_000 });

// 界面语言（服务端按它给错误说明、分类名这些文字）。先只有中文。
export const interfaceLanguage = "zh";

let refreshing: Promise<Tokens | null> | null = null;

async function withBrowserLock<T>(work: () => Promise<T>): Promise<T> {
  const locks = typeof window !== "undefined" ? window.navigator.locks : undefined;
  if (!locks?.request) {
    return work(); // 没有 Web Locks 的浏览器：只在本页内防并发
  }
  return locks.request("haro-token-refresh", work);
}

// refreshTokens 换一对新令牌；used 是这次失败的请求用的访问令牌（已经被别人换过了就直接用新的）。
// 事件流（stream.ts）不走 axios，401 时也调它，所以导出。
export async function refreshTokens(used: string): Promise<Tokens | null> {
  const current = useSession.getState().tokens;
  if (current && current.accessToken !== used) {
    return current;
  }
  refreshing ??= withBrowserLock(async () => {
    await useSession.persist.rehydrate(); // 拿到锁后重新读：别的标签页可能刚换过
    const latest = useSession.getState().tokens;
    if (!latest) {
      return null;
    }
    if (latest.accessToken !== used) {
      return latest;
    }
    try {
      const response = await axios.post(`${apiBaseURL}/api/v1/auth/refresh`, { refresh_token: latest.refreshToken },
        { timeout: 15_000 });
      const tokens = { accessToken: response.data.access_token as string, refreshToken: response.data.refresh_token as string };
      useSession.getState().signIn(tokens);
      return tokens;
    } catch (error) {
      if (error instanceof AxiosError && error.response?.status === 401) {
        useSession.getState().signOut(); // 刷新令牌也不认了：重新登录
      }
      return null;
    }
  }).finally(() => {
    refreshing = null;
  });
  return refreshing;
}

// apiRequest 是 orval 的 mutator：生成的函数都调它。
export async function apiRequest<T>(config: AxiosRequestConfig): Promise<T> {
  const send = (accessToken?: string) =>
    client.request<T>({
      ...config,
      headers: {
        ...config.headers,
        "Accept-Language": interfaceLanguage,
        ...(accessToken ? { Authorization: `Bearer ${accessToken}` } : {}),
      },
    });
  const tokens = useSession.getState().tokens;
  try {
    return (await send(tokens?.accessToken)).data;
  } catch (error) {
    if (!(error instanceof AxiosError) || error.response?.status !== 401 || !tokens) {
      throw error;
    }
    const renewed = await refreshTokens(tokens.accessToken);
    if (!renewed) {
      throw error;
    }
    return (await send(renewed.accessToken)).data;
  }
}

export default apiRequest;
