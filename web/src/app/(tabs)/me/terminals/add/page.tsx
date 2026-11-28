"use client";

// 添加终端：在终端上运行 haro -pair，把它显示的配对码填进来（服务端核对；码对了这台终端就归这个账号）。
import { Check } from "lucide-react";
import { useTranslations } from "next-intl";
import { useState } from "react";

import { pairTerminal } from "@/api/generated/terminals/terminals";
import { problemMessage } from "@/api/problem";
import { Banner, Button, Card, Page } from "@/components/ui";

export default function AddTerminalPage() {
  const t = useTranslations("terminals");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);
  const submit = async () => {
    setBusy(true);
    setResult(null);
    try {
      const paired = await pairTerminal({ code: code.trim() });
      setResult({ ok: true, text: t("paired", { name: paired.terminal.name }) });
      setCode("");
    } catch (failure) {
      setResult({ ok: false, text: problemMessage(failure) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <Page title={t("pairTitle")} back="/me/terminals">
      <Card title={t("stepRun")}>
        <pre className="rounded-[10px] bg-[var(--code)] p-2.5 text-[13px]">haro -pair</pre>
        <p className="sub mt-2">{t("stepRunHint")}</p>
      </Card>
      <Card title={t("stepCode")}>
        <form
          className="hstack flex-wrap"
          onSubmit={(event) => {
            event.preventDefault();
            if (code.trim()) void submit();
          }}
        >
          <input
            className="input num"
            style={{ maxWidth: 240, fontSize: 20, letterSpacing: ".12em", textTransform: "uppercase" }}
            placeholder={t("pairCode")}
            aria-label={t("pairCode")}
            value={code}
            autoComplete="off"
            onChange={(event) => setCode(event.target.value)}
          />
          <Button kind="solid" type="submit" disabled={busy || !code.trim()}>
            {t("pair")}
          </Button>
        </form>
        <p className="cap mt-2">{t("pairOwner")}</p>
        {result && (
          <div className="mt-3">
            <Banner tone={result.ok ? "accent" : "danger"} icon={result.ok ? Check : undefined}>
              {result.text}
            </Banner>
          </div>
        )}
      </Card>
    </Page>
  );
}
