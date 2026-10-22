import { useEffect, useMemo, useState } from "react";
import { Check, Copy, GitBranch, Link2, LoaderCircle, X } from "lucide-react";
import type { Api } from "../api";
import type { SharedSession } from "../types";
import { buildThread } from "../thread";
import { I18n, resolveLang, translator, useI18n } from "../i18n";
import { relativeTime } from "../format";
import { Thread } from "./Thread";
import { UnifiedDiff } from "./UnifiedDiff";
import { IconButton } from "./Sidebar";

// SharePage is /share/<token>: one conversation, read-only, for whoever has
// the link. It needs no server token and offers nothing to act on.
export function SharePage({ token }: { token: string }) {
  const lang = resolveLang(undefined);
  const i18n = useMemo(() => ({ lang, t: translator(lang) }), [lang]);
  const { t } = i18n;
  const [shared, setShared] = useState<SharedSession | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    document.documentElement.dataset.theme = dark ? "dark" : "light";
    document.documentElement.style.colorScheme = dark ? "dark" : "light";
    fetch(`/api/share/${encodeURIComponent(token)}`)
      .then(async (response) => {
        if (!response.ok) throw new Error(response.status === 404 ? t("shareGone") : response.statusText);
        const body = (await response.json()) as SharedSession;
        setShared(body);
        document.title = body.title || "ccl web";
      })
      .catch((err) => setError(err instanceof Error ? err.message : String(err)));
  }, [token, lang, t]);
  const items = useMemo(() => buildThread(shared?.events || []), [shared]);

  return (
    <I18n.Provider value={i18n}>
      <div className="flex h-full flex-col bg-canvas text-ink">
        <header className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-0.5 border-b border-line px-4 py-2.5">
          <span className="font-display text-[17px] font-medium tracking-tight">ccl web</span>
          {shared && (
            <>
              <span className="min-w-0 flex-1 truncate text-[14px] font-medium">{shared.title || t("untitled")}</span>
              <span className="text-[12px] text-ink-3">
                {[shared.workspace, shared.worktree?.branch, relativeTime(shared.updatedAt, lang)].filter(Boolean).join(" · ")}
              </span>
            </>
          )}
          <span className="rounded-md bg-hover px-2 py-0.5 text-[11.5px] text-ink-3">{t("readOnly")}</span>
        </header>
        {error && <p className="mx-auto mt-10 max-w-md px-4 text-center text-[14px] text-ink-3">{error}</p>}
        {!shared && !error && (
          <div className="grid flex-1 place-items-center text-ink-3">
            <LoaderCircle size={18} className="animate-spin" />
          </div>
        )}
        {shared && (
          <div className="flex min-h-0 flex-1 flex-col">
            {/* The thread's empty state invites a message; a shared page cannot. */}
            {!items.length && <p className="mx-auto mt-16 text-[14px] text-ink-3">{t("shareEmpty")}</p>}
            {items.length > 0 && <Thread
              items={items}
              running={false}
              loading={false}
              workspace={shared.workspace}
              route={[shared.provider || "default", shared.model].filter(Boolean).join(" · ")}
              attachmentURL={(file) => `/api/share/${encodeURIComponent(token)}/uploads/${encodeURIComponent(file)}`}
              onPermission={async () => {}}
            />}
            {shared.diff && shared.diff.diff && (
              <details className="mx-auto mb-4 w-full max-w-[760px] shrink-0 rounded-[10px] border border-line px-3 py-2 max-md:mx-4 max-md:w-auto">
                <summary className="flex cursor-pointer items-center gap-2 text-[13px] text-ink-2">
                  <GitBranch size={14} className="text-ink-3" />
                  {t("shareChanges", { branch: shared.worktree?.branch || "", base: shared.diff.base })}
                  {shared.diff.truncated && <span className="text-ink-3">· {t("truncated")}</span>}
                </summary>
                <div className="mt-2 max-h-[60vh] overflow-auto">
                  <UnifiedDiff diff={shared.diff.diff} />
                </div>
              </details>
            )}
          </div>
        )}
      </div>
    </I18n.Provider>
  );
}

// ShareDialog makes, copies and revokes a conversation's read-only link.
export function ShareDialog({ api, sessionId, token, onChanged, onClose }: {
  api: Api;
  sessionId: string;
  // The link it has now, if any.
  token?: string;
  onChanged: () => void;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);
  const url = token ? `${location.origin}/share/${token}` : "";
  const loopback = /^(localhost|127\.|\[::1\])/.test(location.hostname);
  const act = async (action: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await action();
      onChanged();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => event.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <div className="fixed inset-0 z-40 grid place-items-center bg-black/50 p-3" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div role="dialog" aria-label={t("share")} className="w-full max-w-[520px] rounded-[14px] border border-line bg-canvas p-5 shadow-2xl">
        <div className="mb-3 flex items-center gap-2">
          <Link2 size={17} className="text-ink-3" />
          <h2 className="mr-auto text-[16px] font-medium text-ink">{t("shareTitle")}</h2>
          <IconButton title={t("close")} onClick={onClose}>
            <X size={17} />
          </IconButton>
        </div>
        <p className="text-[13px] leading-relaxed text-ink-2">{t("shareHint")}</p>
        {loopback && <p className="mt-2 rounded-lg bg-amber-500/10 px-3 py-2 text-[12.5px] text-ink-2">{t("shareLoopback")}</p>}
        {url ? (
          <div className="mt-4 flex gap-2">
            <input readOnly value={url} onFocus={(event) => event.target.select()} className="h-9 min-w-0 flex-1 rounded-lg border border-line bg-surface px-2.5 font-mono text-[12px] text-ink outline-none" />
            <button
              type="button"
              onClick={() => {
                void navigator.clipboard?.writeText(url).then(() => setCopied(true));
              }}
              className="flex h-9 shrink-0 items-center gap-1.5 rounded-lg border border-line px-3 text-[13px] text-ink-2 hover:bg-hover hover:text-ink"
            >
              {copied ? <Check size={14} /> : <Copy size={14} />}
              {copied ? t("copied") : t("copy")}
            </button>
          </div>
        ) : null}
        {error && <p className="mt-3 rounded-lg bg-red-500/10 px-3 py-2 text-[12.5px] text-red-500">{error}</p>}
        <div className="mt-5 flex justify-end gap-2">
          {url ? (
            <button
              type="button"
              disabled={busy}
              onClick={() => void act(() => api.unshare(sessionId))}
              className="h-9 rounded-lg px-3 text-[13px] text-red-500 hover:bg-red-500/10 disabled:opacity-40"
            >
              {t("stopSharing")}
            </button>
          ) : (
            <button
              type="button"
              disabled={busy}
              onClick={() => void act(() => api.share(sessionId))}
              className="flex h-9 items-center gap-1.5 rounded-lg bg-clay px-3 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
            >
              {busy && <LoaderCircle size={13} className="animate-spin" />}
              {t("createLink")}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
