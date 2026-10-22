import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { LoaderCircle, Plus, ShieldCheck, X } from "lucide-react";
import { HttpError } from "../api";
import type { Api } from "../api";
import { useI18n } from "../i18n";
import type { MessageKey } from "../i18n";
import { readOptional } from "./MemoryEditor";
import type { Loaded } from "./MemoryEditor";

// PermissionRules edits permissions.allow / ask / deny in a project's
// .claude/settings.json or settings.local.json. Every change is saved at
// once against the hash it was loaded with; the rest of the file is kept.

const LISTS = ["allow", "ask", "deny"] as const;
type List = (typeof LISTS)[number];
const LIST_LABEL: Record<List, MessageKey> = { allow: "ruleAllow", ask: "ruleAsk", deny: "ruleDeny" };
const FILES = [
  { path: ".claude/settings.json", label: "settings.json", hint: "settingsShared" },
  { path: ".claude/settings.local.json", label: "settings.local.json", hint: "settingsLocal" },
] as const;

// A rule is a tool name, optionally with a specifier: Bash(npm test:*).
const RULE = /^[A-Za-z][\w-]*(\(.+\))?$/;

type Settings = Record<string, unknown> & { permissions?: Record<string, unknown> };

function rulesOf(settings: Settings, list: List): string[] {
  const value = settings.permissions?.[list];
  return Array.isArray(value) ? value.filter((rule): rule is string => typeof rule === "string") : [];
}

export function PermissionRules({ api }: { api: Api }) {
  const { t } = useI18n();
  const [which, setWhich] = useState(0);
  const target = FILES[which];
  const [file, setFile] = useState<Loaded | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [list, setList] = useState<List>("allow");
  const [rule, setRule] = useState("");

  const load = async () => {
    setFile(null);
    setSettings(null);
    setError("");
    try {
      const loaded = (await readOptional(api, target.path)) || { path: target.path, hash: "", content: "", size: 0 };
      setFile(loaded);
      if (loaded.tooBig) throw new Error(t("invalidSettings", { file: target.label }));
      const parsed: unknown = loaded.content.trim() ? JSON.parse(loaded.content) : {};
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error(t("invalidSettings", { file: target.label }));
      setSettings(parsed as Settings);
    } catch (err) {
      setError(err instanceof SyntaxError ? t("invalidSettings", { file: target.label }) : err instanceof Error ? err.message : String(err));
    }
  };
  useEffect(() => {
    void load();
    // The component remounts for another project.
  }, [which]);

  const change = async (name: List, rules: string[]) => {
    if (!file || !settings || busy) return;
    const permissions = { ...(settings.permissions || {}) };
    if (rules.length) permissions[name] = rules;
    else delete permissions[name];
    const next: Settings = { ...settings, permissions };
    if (!Object.keys(permissions).length) delete next.permissions;
    setBusy(true);
    setError("");
    try {
      const content = JSON.stringify(next, null, 2) + "\n";
      const result = await api.writeFile(file.path, content, file.hash);
      setFile({ ...file, hash: result.hash, content });
      setSettings(next);
      setRule("");
    } catch (err) {
      if (err instanceof HttpError && err.status === 409) {
        // Someone else changed the file: show theirs, then let the user retry.
        await load();
        setError(t("rulesReloaded"));
      } else setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const add = (event: FormEvent) => {
    event.preventDefault();
    const value = rule.trim();
    if (!settings || !value) return;
    if (!RULE.test(value)) {
      setError(t("ruleFormat"));
      return;
    }
    const current = rulesOf(settings, list);
    if (current.includes(value)) {
      setRule("");
      return;
    }
    void change(list, [...current, value]);
  };

  return (
    <section className="flex min-w-0 flex-col rounded-xl border border-line bg-surface">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
        <ShieldCheck size={15} className="shrink-0 text-ink-3" />
        <h2 className="mr-auto text-[14px] font-medium text-ink">{t("permissionRules")}</h2>
        <div className="flex rounded-lg bg-hover p-0.5 text-[12.5px]">
          {FILES.map((item, index) => (
            <button
              key={item.path}
              type="button"
              onClick={() => setWhich(index)}
              className={"rounded-md px-2 py-0.5 font-mono " + (which === index ? "bg-surface text-ink shadow-sm" : "text-ink-3 hover:text-ink")}
            >
              {item.label}
            </button>
          ))}
        </div>
      </div>
      <p className="px-4 pt-3 text-[12.5px] leading-relaxed text-ink-3">
        {t("rulesHint")} {t(target.hint)}
      </p>
      <div className="space-y-3 px-4 py-3">
        {!file && !error && (
          <div className="grid h-20 place-items-center text-ink-3">
            <LoaderCircle size={16} className="animate-spin" />
          </div>
        )}
        {settings &&
          LISTS.map((name) => {
            const rules = rulesOf(settings, name);
            return (
              <div key={name}>
                <h3 className="mb-1.5 text-[12.5px] font-medium text-ink-2">{t(LIST_LABEL[name])}</h3>
                {!rules.length && <p className="text-[12.5px] text-ink-3">{t("noRules")}</p>}
                <ul className="flex flex-wrap gap-1.5">
                  {rules.map((item) => (
                    <li key={item} className="flex min-w-0 max-w-full items-center gap-1 rounded-md border border-line bg-canvas py-0.5 pl-2 pr-0.5 font-mono text-[12px] text-ink">
                      <span className="min-w-0 break-all">{item}</span>
                      <button
                        type="button"
                        disabled={busy}
                        title={t("delete")}
                        aria-label={t("delete")}
                        onClick={() => void change(name, rules.filter((other) => other !== item))}
                        className="shrink-0 rounded p-0.5 text-ink-3 hover:bg-hover hover:text-red-500"
                      >
                        <X size={12} />
                      </button>
                    </li>
                  ))}
                </ul>
              </div>
            );
          })}
        {settings && (
          <form onSubmit={add} className="flex flex-wrap gap-2 pt-1">
            <select
              value={list}
              onChange={(event) => setList(event.target.value as List)}
              className="h-8 rounded-lg border border-line bg-canvas px-2 text-[13px] text-ink outline-none"
            >
              {LISTS.map((name) => (
                <option key={name} value={name}>
                  {t(LIST_LABEL[name])}
                </option>
              ))}
            </select>
            <input
              value={rule}
              onChange={(event) => setRule(event.target.value)}
              placeholder="Bash(npm run test:*)"
              list="ccl-permission-tools"
              spellCheck={false}
              className="h-8 min-w-0 flex-1 basis-40 rounded-lg border border-line bg-canvas px-2.5 font-mono text-[12.5px] text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
            />
            <datalist id="ccl-permission-tools">
              {["Bash", "Read", "Edit", "Write", "WebFetch", "WebSearch", "Glob", "Grep", "NotebookEdit", "Task"].map((tool) => (
                <option key={tool} value={tool} />
              ))}
            </datalist>
            <button
              type="submit"
              disabled={busy || !rule.trim()}
              className="flex h-8 items-center gap-1 rounded-lg border border-line px-3 text-[13px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-40"
            >
              {busy ? <LoaderCircle size={13} className="animate-spin" /> : <Plus size={13} />}
              {t("addRule")}
            </button>
          </form>
        )}
      </div>
      {error && <p className="mx-4 mb-3 rounded-lg bg-red-500/10 px-3 py-2 text-[12.5px] text-red-500">{error}</p>}
    </section>
  );
}
