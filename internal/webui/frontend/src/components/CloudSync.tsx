import { useCallback, useEffect, useState } from "react";
import { Cloud, CloudDownload, CloudUpload, ExternalLink, LoaderCircle, LogOut, RefreshCw, Star } from "lucide-react";
import { HttpError } from "../api";
import type { Api } from "../api";
import type { CloudOverview, CloudRemote } from "../types";
import { relativeTime } from "../format";
import { useI18n } from "../i18n";
import type { MessageKey } from "../i18n";

const control =
  "rounded-lg border border-line bg-surface px-3 py-1.5 text-[14px] text-ink outline-none focus:border-line-strong [&>option]:bg-surface";
const smallButton =
  "flex h-7 items-center gap-1.5 rounded-md border border-line px-2 text-[12.5px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-40";

const STATE_LABEL: Record<string, MessageKey> = {
  empty: "cloudStateEmpty",
  "local only": "cloudStateLocalOnly",
  "up to date": "cloudStateUpToDate",
  "not synchronized": "cloudStateNotSynced",
  diverged: "cloudStateDiverged",
  "remote rollback suspected": "cloudStateRollback",
  "remote changes available": "cloudStateRemoteChanges",
  "local changes pending": "cloudStateLocalChanges",
};

const PLATFORM_LABEL: Record<string, string> = { "google-drive": "Google Drive", icloud: "iCloud" };

type Login = { id: string; output: string[]; urls: string[]; done: boolean; ok: boolean; cancelled?: boolean; error?: string };
type Outcome = { ok: boolean; text: string; output?: string; retry?: { action: "push" | "pull"; remote: string } };

// CloudSync is `ccl cloud` in the settings dialog: the remotes with their
// state, push and pull, and logging in to iCloud or Google Drive. Every
// change runs ccl itself on the server.
export function CloudSync({ api, onPulled }: { api: Api; onPulled: () => void }) {
  const { t, lang } = useI18n();
  const [overview, setOverview] = useState<CloudOverview | null>(null);
  const [loadError, setLoadError] = useState("");
  const [busy, setBusy] = useState("");
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [platform, setPlatform] = useState("google-drive");
  const [alias, setAlias] = useState("");
  const [login, setLogin] = useState<Login | null>(null);
  const [pasted, setPasted] = useState("");

  const load = useCallback(async () => {
    setLoadError("");
    try {
      const next = await api.cloud();
      setOverview(next);
      // Start on a platform this build can log in to.
      setPlatform((current) => (next.unavailable?.[current] ? next.platforms.find((name) => !next.unavailable?.[name]) || current : current));
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    }
  }, [api]);
  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!login || login.done) return;
    const timer = setInterval(async () => {
      try {
        const status = await api.cloudLoginStatus(login.id);
        setLogin(status);
        if (status.done) void load();
      } catch (err) {
        setLoadError(err instanceof Error ? err.message : String(err));
      }
    }, 800);
    return () => clearInterval(timer);
  }, [login, api, load]);

  const run = async (action: "push" | "pull" | "primary" | "logout", remote: string, force = false) => {
    setBusy(action + remote);
    setOutcome(null);
    try {
      const result = await api.cloudCommand(action, remote, force);
      const done = { push: "cloudPushed", pull: "cloudPulled", primary: "cloudMadePrimary", logout: "cloudLoggedOut" } as const;
      setOutcome({ ok: true, text: t(done[action]), output: result.output });
      if (action === "pull") onPulled();
    } catch (err) {
      const output = err instanceof HttpError && err.body && typeof err.body === "object" ? String((err.body as { output?: string }).output || "") : "";
      setOutcome({
        ok: false,
        text: err instanceof Error ? err.message : String(err),
        output,
        retry: err instanceof HttpError && err.status === 409 && (action === "push" || action === "pull") && !force ? { action, remote } : undefined,
      });
    } finally {
      setBusy("");
      void load();
    }
  };

  const startLogin = async (which: string, name: string) => {
    setOutcome(null);
    setPasted("");
    try {
      const { id } = await api.cloudLogin(which, name);
      setLogin({ id, output: [], urls: [], done: false, ok: false });
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : String(err));
    }
  };

  const remotes = overview?.remotes || [];
  return (
    <div className="space-y-5">
      <p className="text-[13px] leading-relaxed text-ink-3">{t("cloudHint")}</p>
      {loadError && <p className="rounded-lg bg-red-500/10 px-3 py-2 text-[12.5px] text-red-500">{loadError}</p>}

      <section className="space-y-2">
        <div className="flex items-center gap-2">
          <h4 className="mr-auto text-[13px] font-medium text-ink-2">{t("cloudRemotes")}</h4>
          <button type="button" onClick={() => void load()} title={t("reload")} aria-label={t("reload")} className="rounded-md p-1 text-ink-3 hover:bg-hover hover:text-ink">
            <RefreshCw size={14} />
          </button>
        </div>
        {!overview && !loadError && <LoaderCircle size={16} className="animate-spin text-ink-3" />}
        {overview && !remotes.length && <p className="text-[13px] text-ink-3">{t("cloudNone")}</p>}
        {remotes.map((remote) => (
          <RemoteCard
            key={remote.alias}
            remote={remote}
            busy={busy}
            lang={lang}
            onRun={(action) => {
              if (action === "logout" && !confirm(t("cloudConfirmLogout", { remote: remote.alias }))) return;
              void run(action, remote.alias);
            }}
            onLogin={() => void startLogin(remote.provider, remote.alias)}
          />
        ))}
        {outcome && (
          <div className={"rounded-lg px-3 py-2 text-[12.5px] " + (outcome.ok ? "bg-emerald-500/10 text-ink-2" : "bg-red-500/10 text-red-500")}>
            <div className="flex flex-wrap items-center gap-2">
              <span className="mr-auto">{outcome.text}</span>
              {outcome.retry && (
                <button
                  type="button"
                  onClick={() => {
                    const retry = outcome.retry!;
                    if (confirm(t(retry.action === "push" ? "cloudConfirmForcePush" : "cloudConfirmForcePull"))) void run(retry.action, retry.remote, true);
                  }}
                  className="rounded-md border border-red-500/40 px-2 py-0.5 text-[12px] text-red-500 hover:bg-red-500/10"
                >
                  {t(outcome.retry.action === "push" ? "cloudForcePush" : "cloudForcePull")}
                </button>
              )}
            </div>
            {outcome.output && (
              <details className="mt-1">
                <summary className="cursor-pointer text-ink-3">{t("cloudOutput")}</summary>
                <pre className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap font-mono text-[11.5px] text-ink-3">{outcome.output}</pre>
              </details>
            )}
          </div>
        )}
      </section>

      <section className="space-y-2">
        <h4 className="text-[13px] font-medium text-ink-2">{t("cloudAdd")}</h4>
        <div className="flex flex-wrap items-center gap-2">
          <select value={platform} onChange={(event) => setPlatform(event.target.value)} className={control}>
            {(overview?.platforms || ["google-drive"]).map((name) => (
              <option key={name} value={name}>{PLATFORM_LABEL[name] || name}</option>
            ))}
          </select>
          <input value={alias} onChange={(event) => setAlias(event.target.value)} placeholder={t("cloudAlias")} className={control + " w-40"} />
          <button
            type="button"
            onClick={() => void startLogin(platform, alias.trim())}
            disabled={(!!login && !login.done) || overview?.available === false || !!overview?.unavailable?.[platform]}
            className="rounded-lg border border-clay bg-clay px-3.5 py-1.5 text-sm font-medium text-white hover:bg-clay-hover disabled:opacity-50"
          >
            {t("loginStart")}
          </button>
          {login && !login.done && (
            <button type="button" onClick={() => api.cloudLoginCancel(login.id).catch(() => {})} className="rounded-lg border border-line px-3 py-1.5 text-sm text-ink-2 hover:bg-hover">
              {t("cancel")}
            </button>
          )}
        </div>
        {overview?.unavailable?.[platform] && (
          <p className="rounded-lg bg-amber-500/10 px-3 py-2 text-[12.5px] leading-relaxed text-ink-2" title={overview.unavailable[platform]}>
            {platform === "google-drive" ? t("cloudGoogleUnavailable") : overview.unavailable[platform]}
          </p>
        )}
        {login && (
          <div className="space-y-2 rounded-lg border border-line bg-code p-2.5 text-[12.5px]">
            <div className="flex items-center gap-1.5 text-ink-2">
              {!login.done && <LoaderCircle size={13} className="animate-spin" />}
              {login.done ? (login.ok ? t("loginDone") : login.cancelled ? t("loginCancelled") : t("loginFailed", { error: login.error || "" })) : t("loginWaiting")}
            </div>
            {!login.done && login.urls.slice(0, 1).map((url) => (
              <div key={url} className="space-y-2">
                <a href={url} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1.5 rounded-lg bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover">
                  <ExternalLink size={13} className="shrink-0" /> {t("loginOpen")}
                </a>
                <p className="text-ink-3">{t("cloudPasteHint")}</p>
                <form
                  className="flex gap-2"
                  onSubmit={(event) => {
                    event.preventDefault();
                    if (!pasted.trim()) return;
                    api.cloudLoginPaste(login.id, pasted.trim()).then(() => setPasted(""), (err) => setLoadError(err instanceof Error ? err.message : String(err)));
                  }}
                >
                  <input value={pasted} onChange={(event) => setPasted(event.target.value)} placeholder="http://127.0.0.1:…/oauth2/callback?…" className={control + " min-w-0 flex-1 font-mono text-[12px]"} />
                  <button type="submit" className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-ink-2 hover:bg-hover">{t("cloudSubmit")}</button>
                </form>
              </div>
            ))}
            <pre className="max-h-32 overflow-auto whitespace-pre-wrap font-mono text-ink-3">{login.output.join("\n")}</pre>
          </div>
        )}
      </section>
    </div>
  );
}

function RemoteCard({ remote, busy, lang, onRun, onLogin }: {
  remote: CloudRemote;
  busy: string;
  lang: "zh" | "en";
  onRun: (action: "push" | "pull" | "primary" | "logout") => void;
  onLogin: () => void;
}) {
  const { t } = useI18n();
  const working = (action: string) => busy === action + remote.alias;
  const disabled = !!busy;
  const state = remote.state ? (STATE_LABEL[remote.state] ? t(STATE_LABEL[remote.state]) : remote.state) : "";
  return (
    <div className="rounded-[10px] border border-line px-3 py-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <Cloud size={15} className="shrink-0 text-ink-3" />
        <span className="text-[14px] font-medium text-ink">{remote.alias}</span>
        <span className="text-[12.5px] text-ink-3">{PLATFORM_LABEL[remote.provider] || remote.provider}</span>
        {remote.primary && <span className="rounded bg-clay/10 px-1.5 text-[11.5px] text-clay">{t("cloudPrimary")}</span>}
        {remote.accountHint && <span className="truncate text-[12px] text-ink-3">{remote.accountHint}</span>}
      </div>
      <p className="mt-1 text-[12.5px] text-ink-2">
        {!remote.signedIn ? (
          <span className="text-amber-600">{t("cloudSignedOut")}</span>
        ) : remote.error ? (
          <span className="text-red-500">{remote.error}</span>
        ) : (
          <>
            {state}
            {remote.lastSyncAt && !remote.lastSyncAt.startsWith("0001") && (
              <span className="text-ink-3"> · {t("cloudLastSync", { time: relativeTime(remote.lastSyncAt, lang), op: remote.lastOperation === "push" ? t("cloudPush") : remote.lastOperation === "pull" ? t("cloudPull") : remote.lastOperation || "" })}</span>
            )}
            {remote.remoteCreated && !remote.remoteCreated.startsWith("0001") && (
              <span className="text-ink-3">
                {" · "}
                {t("cloudUploaded", {
                  time: relativeTime(remote.remoteCreated, lang),
                  device: remote.remoteDevice && remote.remoteDevice === remote.device ? t("cloudThisDevice") : (remote.remoteDevice || "?").slice(0, 8),
                })}
              </span>
            )}
          </>
        )}
      </p>
      <div className="mt-2 flex flex-wrap gap-1.5">
        {remote.signedIn ? (
          <>
            <button type="button" disabled={disabled} onClick={() => onRun("push")} className={smallButton}>
              {working("push") ? <LoaderCircle size={13} className="animate-spin" /> : <CloudUpload size={13} />} {t("cloudPush")}
            </button>
            <button type="button" disabled={disabled} onClick={() => onRun("pull")} className={smallButton}>
              {working("pull") ? <LoaderCircle size={13} className="animate-spin" /> : <CloudDownload size={13} />} {t("cloudPull")}
            </button>
            {!remote.primary && (
              <button type="button" disabled={disabled} onClick={() => onRun("primary")} className={smallButton}>
                <Star size={13} /> {t("cloudMakePrimary")}
              </button>
            )}
            <button type="button" disabled={disabled} onClick={() => onRun("logout")} className={smallButton}>
              <LogOut size={13} /> {t("cloudLogout")}
            </button>
          </>
        ) : (
          <button type="button" disabled={disabled} onClick={onLogin} className={smallButton}>
            {t("loginStart")}
          </button>
        )}
      </div>
    </div>
  );
}
