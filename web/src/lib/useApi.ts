"use client";

// 读一个接口：加载中 / 出错（服务端写好的说明）/ 数据，reload 重新读。load 要是稳定的函数（模块里的函数或 useCallback）。
import { useCallback, useEffect, useState } from "react";

import { problemMessage } from "@/api/problem";

export interface Loaded<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

export function useApi<T>(load: () => Promise<T>): Loaded<T> {
  const [state, setState] = useState<{ data: T | null; error: string | null; loading: boolean }>({
    data: null,
    error: null,
    loading: true,
  });
  const reload = useCallback(() => {
    load().then(
      (data) => setState({ data, error: null, loading: false }),
      (error: unknown) => setState((previous) => ({ ...previous, error: problemMessage(error), loading: false })),
    );
  }, [load]);
  useEffect(reload, [reload]);
  return { ...state, reload };
}

// useAction 包一个会改东西的请求：进行中不能再点；出错时给服务端写好的说明，成功后调 done（例如重新读列表）。
export function useAction<A extends unknown[]>(run: (...args: A) => Promise<unknown>, done?: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const act = useCallback(
    async (...args: A) => {
      setBusy(true);
      setError(null);
      try {
        await run(...args);
        done?.();
        return true;
      } catch (failure) {
        setError(problemMessage(failure));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [run, done],
  );
  return { act, busy, error };
}
