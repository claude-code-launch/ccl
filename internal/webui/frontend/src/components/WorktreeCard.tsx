import { useCallback, useEffect, useState } from "react";
import { GitBranch, GitMerge, LoaderCircle, Upload } from "lucide-react";
import type { Api } from "../api";
import type { WorktreeStatus } from "../types";
import { useI18n } from "../i18n";

// WorktreeCard is the branch a conversation runs on: how far it is from the
// branch it started from, and the two ways its work leaves the worktree —
// a merge into that branch in the project, or a push.
export function WorktreeCard({ api, sessionId, refreshKey, running }: { api: Api; sessionId: string; refreshKey: number; running: boolean }) {
  const { t } = useI18n();
  const [status, setStatus] = useState<WorktreeStatus | null>(null);
  const [busy, setBusy] = useState<"" | "merge" | "push">("");
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);

  const load = useCallback(async () => {
    try {
      setStatus(await api.worktreeStatus(sessionId));
    } catch (error) {
      setMessage({ ok: false, text: error instanceof Error ? error.message : String(error) });
    }
  }, [api, sessionId]);
  useEffect(() => {
    void load();
  }, [load, refreshKey]);

  const act = async (action: "merge" | "push") => {
    setBusy(action);
    setMessage(null);
    try {
      const result = await api.worktreeAction(sessionId, action);
      setStatus(result.status);
      setMessage({ ok: true, text: action === "merge" ? t("merged", { base: result.status.base || "" }) : t("pushed", { remote: result.status.remote || "" }) });
    } catch (error) {
      setMessage({ ok: false, text: error instanceof Error ? error.message : String(error) });
      void load();
    } finally {
      setBusy("");
    }
  };

  if (!status) return null;
  const target = status.base || status.baseCommit.slice(0, 7);
  // Why a merge cannot happen now, if it cannot.
  const mergeBlock = !status.base
    ? t("mergeDetached")
    : status.current !== status.base
      ? t("mergeCheckout", { base: status.base })
      : status.dirty
        ? t("mergeDirty")
        : !status.ahead
          ? t("mergeNothing")
          : "";
  return (
    <section className="rounded-[10px] border border-line px-2 py-1.5">
      <h3 className="px-1 py-1 text-[13px] text-ink-2">{t("worktreeBranch")}</h3>
      <div className="flex min-w-0 items-center gap-2 px-1 py-1 text-[13px] text-ink">
        <GitBranch size={15} className="shrink-0 text-ink-3" />
        <span className="min-w-0 truncate font-mono text-[12.5px]" title={status.path}>{status.branch}</span>
        <span className="shrink-0 text-ink-3">→ {target}</span>
      </div>
      {!status.exists ? (
        <p className="px-1 py-1 text-[12.5px] text-red-500">{t("worktreeMissing")}</p>
      ) : (
        <p className="px-1 py-0.5 text-[12.5px] text-ink-2">
          {t("worktreeCounts", { ahead: status.ahead, behind: status.behind, files: status.files.length })}
          {status.dirty > 0 && <span className="text-amber-500"> · {t("worktreeDirty", { n: status.dirty })}</span>}
          {status.upstream && <span className="text-ink-3"> · {status.upstream}</span>}
        </p>
      )}
      {status.exists && (
        <div className="mt-1.5 flex flex-wrap gap-1.5 px-1">
          <button
            type="button"
            disabled={!!busy || running || !!mergeBlock}
            title={mergeBlock || t("mergeInto", { base: target })}
            onClick={() => void act("merge")}
            className="flex h-7 items-center gap-1.5 rounded-md border border-line px-2 text-[12.5px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-40"
          >
            {busy === "merge" ? <LoaderCircle size={13} className="animate-spin" /> : <GitMerge size={13} />}
            {t("mergeInto", { base: target })}
          </button>
          {status.remote && (
            <button
              type="button"
              disabled={!!busy || running || !status.ahead}
              title={t("pushTo", { remote: status.remote })}
              onClick={() => void act("push")}
              className="flex h-7 items-center gap-1.5 rounded-md border border-line px-2 text-[12.5px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-40"
            >
              {busy === "push" ? <LoaderCircle size={13} className="animate-spin" /> : <Upload size={13} />}
              {t("pushTo", { remote: status.remote })}
            </button>
          )}
        </div>
      )}
      {mergeBlock && status.exists && status.ahead > 0 && <p className="px-1 pt-1.5 text-[12px] text-ink-3">{mergeBlock}</p>}
      {message && <p className={"mt-1.5 break-words px-1 text-[12px] " + (message.ok ? "text-emerald-600" : "text-red-500")}>{message.text}</p>}
      <div className="pb-1" />
    </section>
  );
}
