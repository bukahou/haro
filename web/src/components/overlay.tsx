"use client";

// 全站一个样式的确认框（屏幕中间的卡，点遮罩 / Esc = 取消）和轻提示。页面里调 openDialog / showToast，外壳里放一个 <OverlayHost />。
import clsx from "clsx";
import { useEffect, useState } from "react";
import { create } from "zustand";

import { problemMessage } from "@/api/problem";

export type ButtonKind = "solid" | "outline" | "plain" | "danger";

export interface DialogButton {
  label: string;
  kind?: ButtonKind;
  // 返回 false 时框不关；抛错时框不关，在框里显示服务端写好的说明
  run?: () => void | boolean | Promise<void | boolean>;
}

export interface DialogSpec {
  title: string;
  body?: React.ReactNode;
  buttons: DialogButton[];
  stacked?: boolean; // 选项竖着排（例如重复日程「只改这一次 / 这次和以后 / 全部」）
}

export interface ToastAction {
  label: string;
  run: () => void | Promise<void>;
}

interface OverlayState {
  dialog: DialogSpec | null;
  toast: { text: string; action?: ToastAction; id: number } | null;
  openDialog: (dialog: DialogSpec) => void;
  closeDialog: () => void;
  showToast: (text: string, action?: ToastAction) => void;
}

const toastMillis = 2400;
const toastWithActionMillis = 6000; // 带「撤销」的多留一会儿

const useOverlay = create<OverlayState>((set) => ({
  dialog: null,
  toast: null,
  openDialog: (dialog) => set({ dialog }),
  closeDialog: () => set({ dialog: null }),
  showToast: (text, action) => set({ toast: { text, action, id: Date.now() } }),
}));

export const openDialog = (dialog: DialogSpec) => useOverlay.getState().openDialog(dialog);
export const closeDialog = () => useOverlay.getState().closeDialog();
export const showToast = (text: string, action?: ToastAction) => useOverlay.getState().showToast(text, action);

// confirmDialog 是最常见的一种：一句说明 + 取消 / 确认（确认那个按钮的样式和文字由调用方给）。
export function confirmDialog(spec: { title: string; body?: React.ReactNode; confirm: string; cancel: string; kind?: ButtonKind; run: DialogButton["run"] }) {
  openDialog({
    title: spec.title,
    body: spec.body,
    buttons: [
      { label: spec.cancel, kind: "plain" },
      { label: spec.confirm, kind: spec.kind ?? "danger", run: spec.run },
    ],
  });
}

function Dialog({ spec }: { spec: DialogSpec }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const press = async (button: DialogButton) => {
    if (!button.run) {
      closeDialog();
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const keep = (await button.run()) === false;
      if (!keep) closeDialog();
    } catch (failure) {
      setError(problemMessage(failure));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="scrim" onClick={(event) => event.target === event.currentTarget && !busy && closeDialog()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={spec.title}>
        <h3>{spec.title}</h3>
        {spec.body && <div className="sub" style={{ fontSize: 14 }}>{spec.body}</div>}
        {error && <div className="banner d">{error}</div>}
        <div className={spec.stacked ? "choices" : "row-btns"}>
          {spec.buttons.map((button) => (
            <button
              key={button.label}
              type="button"
              disabled={busy}
              className={clsx("btn", button.kind && button.kind !== "outline" && button.kind, spec.stacked ? "block" : "sm")}
              onClick={() => void press(button)}
            >
              {button.label}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}

export function OverlayHost() {
  const dialog = useOverlay((state) => state.dialog);
  const toast = useOverlay((state) => state.toast);
  useEffect(() => {
    if (!dialog) return;
    const onKey = (event: KeyboardEvent) => event.key === "Escape" && closeDialog();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [dialog]);
  useEffect(() => {
    if (!toast) return;
    const timer = setTimeout(() => useOverlay.setState({ toast: null }), toast.action ? toastWithActionMillis : toastMillis);
    return () => clearTimeout(timer);
  }, [toast]);
  return (
    <>
      {dialog && <Dialog key={dialog.title} spec={dialog} />}
      {toast && (
        <div className="toast flex items-center gap-3" role="status">
          {toast.text}
          {toast.action && (
            <button
              type="button"
              className="font-semibold underline"
              onClick={() => {
                const action = toast.action;
                useOverlay.setState({ toast: null });
                void action?.run();
              }}
            >
              {toast.action.label}
            </button>
          )}
        </div>
      )}
    </>
  );
}
