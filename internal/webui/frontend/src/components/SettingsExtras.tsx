import { useCallback, useEffect, useState } from "react";
import { ExternalLink, LoaderCircle, Plus, Trash2 } from "lucide-react";
import type { Api } from "../api";
import type { MCPEntry } from "../types";
import { OAUTH_PROVIDERS } from "../types";
import { useI18n } from "../i18n";

const control =
  "rounded-lg border border-line bg-surface px-3 py-1.5 text-[14px] text-ink outline-none focus:border-line-strong [&>option]:bg-surface";

// SubscriptionLogin runs `ccl oauth <name>` on the server and shows the link it
// prints. The login finishes in the browser tab it opens, as in a terminal.
export function SubscriptionLogin({ api, onDone }: { api: Api; onDone: () => void }) {
  const { t } = useI18n();
  const [provider, setProvider] = useState<string>("gpt");
  const [alias, setAlias] = useState("");
  const [login, setLogin] = useState<{ id: string; output: string[]; urls: string[]; done: boolean; ok: boolean; cancelled?: boolean; error?: string } | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!login || login.done) return;
    const timer = setInterval(async () => {
      try {
        const status = await api.oauthStatus(login.id);
        // The link is shown for the user to open: a page opened from a timer
        // would be blocked as a pop-up.
        setLogin(status);
        if (status.done && status.ok) onDone();
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      }
    }, 800);
    return () => clearInterval(timer);
  }, [login, api, onDone]);

  const start = async () => {
    setError("");
    try {
      const { id } = await api.oauthStart(provider, alias.trim());
      setLogin({ id, output: [], urls: [], done: false, ok: false });
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <select value={provider} onChange={(event) => setProvider(event.target.value)} className={control}>
          {OAUTH_PROVIDERS.map((name) => (
            <option key={name} value={name}>{name}</option>
          ))}
        </select>
        <input value={alias} onChange={(event) => setAlias(event.target.value)} placeholder={t("loginAlias")} className={control + " w-40"} />
        <button
          onClick={start}
          disabled={!!login && !login.done}
          className="rounded-lg border border-clay bg-clay px-3.5 py-1.5 text-sm font-medium text-white hover:bg-clay-hover disabled:opacity-50"
        >
          {t("loginStart")}
        </button>
        {login && !login.done && (
          <button onClick={() => api.oauthCancel(login.id).catch(() => {})} className="rounded-lg border border-line px-3 py-1.5 text-sm text-ink-2 hover:bg-hover">
            {t("cancel")}
          </button>
        )}
      </div>
      {login && (
        <div className="rounded-lg border border-line bg-code p-2.5 text-[12.5px]">
          <div className="mb-1 flex items-center gap-1.5 text-ink-2">
            {!login.done && <LoaderCircle size={13} className="animate-spin" />}
            {login.done ? (login.ok ? t("loginDone") : login.cancelled ? t("loginCancelled") : t("loginFailed", { error: login.error || "" })) : t("loginWaiting")}
          </div>
          {!login.done && login.urls.slice(0, 1).map((url) => (
            <a key={url} href={url} target="_blank" rel="noopener noreferrer" className="mb-2 inline-flex items-center gap-1.5 rounded-lg bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover">
              <ExternalLink size={13} className="shrink-0" /> {t("loginOpen")}
            </a>
          ))}
          <pre className="max-h-32 overflow-auto whitespace-pre-wrap font-mono text-ink-3">{login.output.join("\n")}</pre>
        </div>
      )}
      {error && <p className="text-xs text-red-500">{error}</p>}
      <p className="text-xs text-ink-3">{t("loginHint")}</p>
    </div>
  );
}

// MCPServers lists Claude Code's MCP servers for the workspace and changes them
// through `claude mcp`, so they land where Claude Code's own command puts them.
export function MCPServers({ api }: { api: Api }) {
  const { t } = useI18n();
  const [servers, setServers] = useState<MCPEntry[] | null>(null);
  const [claude, setClaude] = useState(true);
  const [error, setError] = useState("");
  const [form, setForm] = useState({ name: "", scope: "local", type: "stdio", command: "", url: "" });

  const load = useCallback(async () => {
    try {
      const result = await api.mcpList();
      setServers(result.servers);
      setClaude(result.claude);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [api]);
  useEffect(() => {
    void load();
  }, [load]);

  const run = async (action: () => Promise<{ servers: MCPEntry[] }>) => {
    setError("");
    try {
      setServers((await action()).servers);
      return true;
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      return false;
    }
  };

  const add = async () => {
    // "npx -y pkg --flag" splits into a command and its arguments.
    const [command, ...args] = form.command.trim().split(/\s+/);
    const ok = await run(() =>
      api.mcpAdd(form.type === "stdio" ? { name: form.name.trim(), scope: form.scope, type: "stdio", command, args } : { name: form.name.trim(), scope: form.scope, type: form.type, url: form.url.trim() }),
    );
    if (ok) setForm({ ...form, name: "", command: "", url: "" });
  };

  return (
    <div className="space-y-2">
      {!claude && <p className="text-xs text-red-500">{t("mcpNoClaude")}</p>}
      <div className="overflow-hidden rounded-[10px] border border-line">
        {servers === null && <LoaderCircle size={14} className="m-3 animate-spin text-ink-3" />}
        {servers?.length === 0 && <p className="px-3 py-2.5 text-[13px] text-ink-3">{t("mcpEmpty")}</p>}
        {servers?.map((server) => (
          <div key={server.scope + server.name} className="flex items-center gap-2 border-t border-line px-3 py-2 text-[13px] first:border-t-0">
            <span className="font-medium text-ink">{server.name}</span>
            <span className="rounded bg-hover px-1.5 text-[11px] text-ink-2">{t(("scope_" + server.scope) as "scope_user")}</span>
            <span className="text-[11px] text-ink-3">{server.type}</span>
            <span className="min-w-0 flex-1 truncate font-mono text-[12px] text-ink-3" title={server.target}>{server.target}</span>
            {server.env?.length ? <span className="shrink-0 text-[11px] text-ink-3" title={server.env.join(", ")}>env ×{server.env.length}</span> : null}
            <button
              title={t("delete")}
              onClick={() => confirm(t("mcpConfirmRemove", { name: server.name })) && void run(() => api.mcpRemove(server.name, server.scope))}
              className="shrink-0 rounded p-1 text-ink-3 hover:bg-hover hover:text-red-500"
            >
              <Trash2 size={13} />
            </button>
          </div>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <input value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder={t("name")} className={control + " w-32"} />
        <select value={form.scope} onChange={(event) => setForm({ ...form, scope: event.target.value })} className={control}>
          {["local", "project", "user"].map((scope) => (
            <option key={scope} value={scope}>{t(("scope_" + scope) as "scope_user")}</option>
          ))}
        </select>
        <select value={form.type} onChange={(event) => setForm({ ...form, type: event.target.value })} className={control}>
          {["stdio", "http", "sse"].map((type) => (
            <option key={type} value={type}>{type}</option>
          ))}
        </select>
        {form.type === "stdio" ? (
          <input value={form.command} onChange={(event) => setForm({ ...form, command: event.target.value })} placeholder="npx -y @modelcontextprotocol/server-filesystem ." className={control + " min-w-[220px] flex-1 font-mono text-[13px]"} />
        ) : (
          <input value={form.url} onChange={(event) => setForm({ ...form, url: event.target.value })} placeholder="https://…/mcp" className={control + " min-w-[220px] flex-1"} />
        )}
        <button
          disabled={!form.name.trim() || !(form.type === "stdio" ? form.command.trim() : form.url.trim())}
          onClick={add}
          className="flex items-center gap-1 rounded-lg border border-line px-3 py-1.5 text-sm text-ink hover:bg-hover disabled:opacity-50"
        >
          <Plus size={14} /> {t("add")}
        </button>
      </div>
      {error && <p className="whitespace-pre-wrap text-xs text-red-500">{error}</p>}
      <p className="text-xs text-ink-3">{t("mcpHint")}</p>
    </div>
  );
}
