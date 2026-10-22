import { useState } from "react";
import { useI18n } from "../i18n";

export function Gate({ error, onSubmit }: { error: string; onSubmit: (token: string) => void }) {
  const { t } = useI18n();
  const [token, setToken] = useState("");
  return (
    <div className="grid h-full place-items-center bg-canvas text-ink">
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (token.trim()) onSubmit(token.trim());
        }}
        className="grid w-[min(400px,90vw)] gap-3 rounded-[14px] border border-line bg-surface p-7"
      >
        <h1 className="font-display text-[26px] font-medium">ccl web</h1>
        <p className="text-[13px] text-ink-3">{t("gateText")}</p>
        <input
          type="password"
          autoFocus
          autoComplete="off"
          value={token}
          onChange={(event) => setToken(event.target.value)}
          placeholder="token"
          className="rounded-lg border border-line bg-canvas px-3 py-2 text-[14px] outline-none focus:border-line-strong"
        />
        <button type="submit" className="rounded-lg bg-clay px-3 py-2 text-[14px] font-medium text-white hover:bg-clay-hover">
          {t("connect")}
        </button>
        {error && <p className="text-[13px] text-red-500">{error}</p>}
      </form>
    </div>
  );
}
