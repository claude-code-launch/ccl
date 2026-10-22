import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { Blocks, FileText, KeyRound, List, LoaderCircle, Pencil, Plus, RefreshCw, Server, SlidersHorizontal, Star, Trash2, Wifi, X } from "lucide-react";
import type { Api } from "../api";
import type { ConfigView, ProviderView, TestResult } from "../types";
import { EFFORT_LEVELS } from "../types";
import { IconButton } from "./Sidebar";
import { useI18n } from "../i18n";
import { MCPServers, SubscriptionLogin } from "./SettingsExtras";
import { MemoryEditor } from "./MemoryEditor";
import type { MemorySource } from "./MemoryEditor";

// Settings: a large centered dialog with a section list, like claude.ai's.
// Every write to ~/.ccl/config.yaml goes through the server's config.Update;
// a provider is created or edited in a dialog of its own.

const PROVIDER_TYPES = ["anthropic", "openai", "openai_responses", "modelsdev", "autoclaw"];
const SLOT_FIELDS = [
  ["opusModel", "opus"],
  ["sonnetModel", "sonnet"],
  ["haikuModel", "haiku"],
  ["fableModel", "fable"],
  ["customModelId", "custom"],
  ["subagentModel", "subagent"],
] as const;

type Tab = "general" | "providers" | "login" | "connectors" | "instructions";
type Form = Record<string, string> & { name: string; type: string; endpoint: string; apiKey: string; effortLevel: string };

const emptyForm = (): Form => ({
  name: "",
  type: "anthropic",
  endpoint: "",
  apiKey: "",
  effortLevel: "",
  opusModel: "",
  sonnetModel: "",
  haikuModel: "",
  fableModel: "",
  customModelId: "",
  subagentModel: "",
});

interface Props {
  api: Api;
  // The client for the open workspace, whose MCP servers are listed.
  workspaceApi: Api;
  config: ConfigView | null;
  loadError: string;
  // The section to open, such as connectors from the composer's menu.
  initialTab?: Tab;
  onClose: () => void;
  onChanged: () => Promise<void>;
}

export function SettingsDialog({ api, workspaceApi, config, loadError, initialTab = "general", onClose: close, onChanged }: Props) {
  const { t } = useI18n();
  const [tab, setTabState] = useState<Tab>(initialTab);
  // The instructions editor asks before unsaved edits are dropped.
  const leaveGuard = useRef<(() => boolean) | null>(null);
  const registerLeaveGuard = useCallback((guard: (() => boolean) | null) => {
    leaveGuard.current = guard;
  }, []);
  const canLeave = () => !leaveGuard.current || leaveGuard.current();
  const onClose = () => canLeave() && close();
  const setTab = (next: Tab) => next !== tab && canLeave() && setTabState(next);
  const userMemory: MemorySource[] = useMemo(
    () => [
      {
        key: "user",
        label: "CLAUDE.md",
        hint: t("memoryUser"),
        load: async () => {
          const file = await api.userMemory();
          return { path: file.path, hash: file.hash, content: file.content, size: file.content.length, tooBig: file.tooBig };
        },
        save: async (file, content) => (await api.saveUserMemory(content, file.hash)).hash,
      },
    ],
    [api, t],
  );
  // null: no editor; "new": creating; a provider: editing it.
  const [editor, setEditor] = useState<ProviderView | "new" | null>(null);

  useEffect(() => {
    // Escape closes the innermost dialog only.
    const onKey = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      if (editor) setEditor(null);
      else onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [editor, onClose]);

  const tabs: { id: Tab; label: string; icon: ReactNode }[] = [
    { id: "general", label: t("general"), icon: <SlidersHorizontal size={16} /> },
    { id: "providers", label: t("providers"), icon: <Server size={16} /> },
    { id: "login", label: t("subscriptionLogin"), icon: <KeyRound size={16} /> },
    { id: "connectors", label: t("connectorsShort"), icon: <Blocks size={16} /> },
    { id: "instructions", label: t("instructions"), icon: <FileText size={16} /> },
  ];
  const current = tabs.find((item) => item.id === tab)!;

  return (
    <div className="fixed inset-0 z-30 grid place-items-center bg-black/50 p-3 sm:p-6" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div
        role="dialog"
        aria-label={t("settings")}
        className="flex h-[min(760px,92vh)] w-full max-w-[980px] overflow-hidden rounded-[16px] border border-line bg-canvas text-ink shadow-2xl max-md:flex-col"
      >
        <nav className="flex w-[220px] shrink-0 flex-col gap-0.5 border-r border-line bg-sidebar p-3 max-md:w-full max-md:flex-row max-md:overflow-x-auto max-md:border-b max-md:border-r-0">
          <h2 className="font-display px-2 pb-3 pt-1 text-[20px] font-medium max-md:hidden">{t("settings")}</h2>
          {tabs.map((item) => (
            <button
              key={item.id}
              onClick={() => setTab(item.id)}
              className={
                "flex h-9 shrink-0 items-center gap-2.5 rounded-lg px-2.5 text-left text-[14px] " +
                (tab === item.id ? "bg-active text-ink" : "text-ink-2 hover:bg-hover hover:text-ink")
              }
            >
              <span className="text-ink-3">{item.icon}</span>
              {item.label}
            </button>
          ))}
        </nav>
        <section className="flex min-h-0 min-w-0 flex-1 flex-col">
          <header className="flex h-14 shrink-0 items-center justify-between border-b border-line px-6">
            <h3 className="text-[16px] font-medium">{current.label}</h3>
            <IconButton title={t("close")} onClick={onClose}>
              <X size={18} />
            </IconButton>
          </header>
          <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5">
            {loadError && <p className="mb-4 rounded-lg bg-red-500/10 px-3 py-2 text-[13px] text-red-500">{t("couldNotLoad", { error: loadError })}</p>}
            {tab === "general" && <General api={api} config={config} onChanged={onChanged} />}
            {tab === "providers" && <Providers api={api} config={config} onChanged={onChanged} onEdit={setEditor} />}
            {tab === "login" && <SubscriptionLogin api={api} onDone={() => void onChanged()} />}
            {tab === "connectors" && <MCPServers api={workspaceApi} />}
            {tab === "instructions" && <MemoryEditor title={t("userInstructions")} sources={userMemory} registerLeaveGuard={registerLeaveGuard} framed={false} />}
          </div>
        </section>
      </div>
      {editor && (
        <ProviderEditor
          api={api}
          provider={editor === "new" ? null : editor}
          onClose={() => setEditor(null)}
          onSaved={async () => {
            await onChanged();
            setEditor(null);
          }}
        />
      )}
    </div>
  );
}

function General({ api, config, onChanged }: { api: Api; config: ConfigView | null; onChanged: () => Promise<void> }) {
  const { t } = useI18n();
  const [active, setActive] = useState("default");
  const [acp, setAcp] = useState("default");
  const [lang, setLang] = useState("");
  const [bypass, setBypass] = useState(false);
  const [status, setStatus] = useState("");
  useEffect(() => {
    if (!config) return;
    setActive(config.active?.normal || "default");
    setAcp(config.active?.acp || "default");
    setLang(config.lang || "");
    setBypass(!!config.bypassMode);
  }, [config]);
  const names = ["default", ...Object.keys(config?.providers || {}).sort()];
  const save = async () => {
    try {
      await api.putConfig({ active: { normal: active, acp }, lang, bypassMode: bypass });
      await onChanged();
      setStatus(t("saved"));
    } catch (error) {
      setStatus(error instanceof Error ? error.message : String(error));
    }
  };
  return (
    <div className="max-w-[560px] space-y-4">
      <Field label={t("activeProvider")} hint={t("activeProviderHint")}>
        <Select value={active} onChange={setActive} options={names} />
      </Field>
      <Field label={t("acpProvider")}>
        <Select value={acp} onChange={setAcp} options={names} />
      </Field>
      <Field label={t("language")}>
        <Select value={lang} onChange={setLang} options={["", "zh", "en"]} labels={{ "": t("langAuto"), zh: "中文", en: "English" }} />
      </Field>
      <Field label={t("bypass")} hint={t("bypassHint")}>
        <input type="checkbox" checked={bypass} onChange={(event) => setBypass(event.target.checked)} className="size-4 accent-[var(--clay)]" />
      </Field>
      <div className="flex items-center gap-3 pt-2">
        <Button primary onClick={save}>{t("save")}</Button>
        <Status text={status} />
      </div>
    </div>
  );
}

function Providers({ api, config, onChanged, onEdit }: {
  api: Api;
  config: ConfigView | null;
  onChanged: () => Promise<void>;
  onEdit: (provider: ProviderView | "new") => void;
}) {
  const { t } = useI18n();
  const [statuses, setStatuses] = useState<Record<string, string>>({});
  const providers = Object.values(config?.providers || {}).sort((a, b) => a.name.localeCompare(b.name));
  const setStatus = (name: string, text: string) => setStatuses((all) => ({ ...all, [name]: text }));

  const test = async (name: string) => {
    setStatus(name, t("testing", { name }));
    try {
      setStatus(name, describe(await api.testProvider(name), t));
    } catch (error) {
      setStatus(name, error instanceof Error ? error.message : String(error));
    }
  };
  const use = async (name: string) => {
    try {
      await api.putConfig({ active: { normal: name, acp: name } });
      await onChanged();
      setStatus(name, t("nowActive", { name }));
    } catch (error) {
      setStatus(name, error instanceof Error ? error.message : String(error));
    }
  };
  const remove = async (name: string) => {
    if (!confirm(t("confirmDeleteProvider", { name }))) return;
    try {
      await api.deleteProvider(name);
      await onChanged();
    } catch (error) {
      setStatus(name, error instanceof Error ? error.message : String(error));
    }
  };

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <p className="flex-1 text-[13px] text-ink-3">{t("defaultNote")}</p>
        <Button primary onClick={() => onEdit("new")}>
          <Plus size={14} /> {t("newProvider")}
        </Button>
      </div>
      {!providers.length && <p className="rounded-[10px] border border-dashed border-line px-4 py-6 text-center text-[13px] text-ink-3">{t("noProviders")}</p>}
      {providers.map((provider) => (
        <div key={provider.name} className="rounded-[12px] border border-line px-4 py-3">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[15px] font-medium text-ink">{provider.name}</span>
            {provider.active && <span className="rounded bg-clay/15 px-1.5 text-[11px] text-clay">{t("active")}</span>}
            {provider.acpActive && <span className="rounded bg-hover px-1.5 text-[11px] text-ink-2">ACP</span>}
            <span className="ml-auto flex items-center gap-0.5">
              <IconButton title={t("test")} onClick={() => test(provider.name)}><Wifi size={15} /></IconButton>
              {!provider.active && <IconButton title={t("use")} onClick={() => use(provider.name)}><Star size={15} /></IconButton>}
              <IconButton title={t("edit")} onClick={() => onEdit(provider)}><Pencil size={15} /></IconButton>
              <IconButton title={t("delete")} onClick={() => remove(provider.name)}><Trash2 size={15} /></IconButton>
            </span>
          </div>
          <div className="mt-1 flex flex-wrap gap-x-4 gap-y-0.5 text-[12.5px] text-ink-3">
            <span>{provider.type || "—"}</span>
            {provider.endpoint && <span className="max-w-[320px] truncate" title={provider.endpoint}>{provider.endpoint}</span>}
            <span className="font-mono">{provider.hasKey ? provider.apiKey : t("noKey")}</span>
            <span>{t("modelCount", { n: provider.modelChoices?.length || 0 })}</span>
            {provider.effortLevel && <span>{t("effort")}: {provider.effortLevel}</span>}
          </div>
          {statuses[provider.name] && <p className="mt-1.5 text-xs text-ink-2">{statuses[provider.name]}</p>}
        </div>
      ))}
    </div>
  );
}

function describe(result: TestResult, t: ReturnType<typeof useI18n>["t"]): string {
  return result.ok
    ? t("testOk", { ms: result.elapsedMs }) + (result.models?.length ? " · " + result.models.slice(0, 8).join(", ") : "")
    : t("testFailed", { message: result.message || "unknown error" });
}

// ProviderEditor creates a provider or edits one, in its own dialog.
function ProviderEditor({ api, provider, onClose, onSaved }: {
  api: Api;
  provider: ProviderView | null;
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const { t } = useI18n();
  const [form, setForm] = useState<Form>(() => {
    const next = emptyForm();
    if (provider) {
      next.name = provider.name;
      next.type = provider.type || "anthropic";
      next.endpoint = provider.endpoint || "";
      next.effortLevel = provider.effortLevel || "";
      for (const [field] of SLOT_FIELDS) next[field] = (provider[field] as string) || "";
    }
    return next;
  });
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  // catalog is what the endpoint's /models offered, for the slot pickers.
  const [catalog, setCatalog] = useState<string[]>([]);
  const [catalogState, setCatalogState] = useState<"idle" | "loading" | "done" | "failed">("idle");
  const set = (field: string) => (value: string) => setForm((current) => ({ ...current, [field]: value }));

  const probe = async () => {
    const result = await api.testProvider(form.name.trim() || "new", {
      endpoint: form.endpoint.trim() || undefined,
      apiKey: form.apiKey.trim() || undefined,
      type: form.type || undefined,
    });
    if (result.ok && result.models?.length) setCatalog(result.models);
    return result;
  };
  const test = async () => {
    if (!form.name.trim()) {
      setStatus(t("saveFirst"));
      return;
    }
    setStatus(t("testing", { name: form.name }));
    try {
      setStatus(describe(await probe(), t));
    } catch (error) {
      setStatus(error instanceof Error ? error.message : String(error));
    }
  };
  const loadCatalog = async () => {
    setCatalogState("loading");
    try {
      const result = await probe();
      setCatalogState(result.ok ? "done" : "failed");
      if (!result.ok) setStatus(describe(result, t));
    } catch (error) {
      setCatalogState("failed");
      setStatus(error instanceof Error ? error.message : String(error));
    }
  };
  // An existing provider with an endpoint lists its models at once.
  useEffect(() => {
    if (provider?.endpoint) void loadCatalog();
    // Only when the editor opens.
  }, []);

  // The pickers offer the catalog, the provider's pool and what is set now.
  const options = [
    ...new Set(
      [...catalog, ...(provider?.models || "").split(","), ...SLOT_FIELDS.map(([field]) => form[field])]
        .map((model) => stripOneM(model.trim()))
        .filter(Boolean),
    ),
  ].sort((a, b) => a.localeCompare(b));

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const name = form.name.trim();
    if (!name) return;
    const body: Record<string, unknown> = { name, type: form.type, endpoint: form.endpoint.trim(), effortLevel: form.effortLevel };
    for (const [field] of SLOT_FIELDS) body[field] = form[field].trim();
    // An empty key field means "keep the stored key".
    if (form.apiKey.trim()) body.apiKey = form.apiKey.trim();
    setBusy(true);
    try {
      if (provider) await api.patchProvider(name, body);
      else await api.createProvider(body);
      await onSaved();
    } catch (error) {
      setStatus(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-40 grid place-items-center bg-black/40 p-4" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <form
        onSubmit={submit}
        role="dialog"
        aria-label={provider ? t("editProvider", { name: provider.name }) : t("newProvider")}
        className="flex max-h-[90vh] w-full max-w-[620px] flex-col overflow-hidden rounded-[16px] border border-line bg-canvas text-ink shadow-2xl"
      >
        <header className="flex h-14 shrink-0 items-center justify-between border-b border-line px-6">
          <h3 className="text-[16px] font-medium">{provider ? t("editProvider", { name: provider.name }) : t("newProvider")}</h3>
          <IconButton title={t("close")} onClick={onClose}>
            <X size={18} />
          </IconButton>
        </header>
        <div className="min-h-0 flex-1 space-y-4 overflow-y-auto px-6 py-5">
          <Field label={t("name")}>
            <Input value={form.name} onChange={set("name")} disabled={!!provider} required autoFocus={!provider} />
          </Field>
          <Field label={t("type")}>
            <Select value={form.type} onChange={set("type")} options={PROVIDER_TYPES} />
          </Field>
          <Field label={t("endpoint")}>
            <Input value={form.endpoint} onChange={set("endpoint")} placeholder="https://…" />
          </Field>
          <Field label={t("apiKey")}>
            <Input type="password" value={form.apiKey} onChange={set("apiKey")} placeholder={provider?.hasKey ? t("keepKey") : t("apiKey")} />
          </Field>
          <div>
            <div className="mb-2 flex items-center gap-2">
              <p className="flex-1 text-[13px] text-ink-2">{t("modelSlots")}</p>
              <span className="text-[12px] text-ink-3">
                {catalogState === "loading" ? t("loadingModels") : catalog.length ? t("modelCount", { n: catalog.length }) : ""}
              </span>
              <button
                type="button"
                onClick={loadCatalog}
                disabled={catalogState === "loading"}
                className="flex items-center gap-1 rounded-md px-2 py-1 text-[12.5px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-50"
              >
                {catalogState === "loading" ? <LoaderCircle size={13} className="animate-spin" /> : <RefreshCw size={13} />}
                {t("fetchModels")}
              </button>
            </div>
            <div className="grid grid-cols-2 gap-x-4 gap-y-2.5 max-sm:grid-cols-1">
              {SLOT_FIELDS.map(([field, label]) => (
                <div key={field} className="flex flex-col gap-1 text-[12.5px] text-ink-3">
                  {label}
                  <SlotPicker value={form[field]} options={options} onChange={set(field)} />
                </div>
              ))}
            </div>
            {!options.length && <p className="mt-2 text-[12px] text-ink-3">{t("modelsHint")}</p>}
          </div>
          <Field label={t("effort")}>
            <Select value={form.effortLevel} onChange={set("effortLevel")} options={["", ...EFFORT_LEVELS]} labels={{ "": t("effortDefault") }} />
          </Field>
          <Status text={status} />
        </div>
        <footer className="flex shrink-0 items-center gap-2 border-t border-line px-6 py-3">
          <Button onClick={test}>
            <Wifi size={14} /> {t("testConnection")}
          </Button>
          <span className="flex-1" />
          <Button onClick={onClose}>{t("cancel")}</Button>
          <Button primary type="submit" disabled={busy}>{t("saveProvider")}</Button>
        </footer>
      </form>
    </div>
  );
}

// stripOneM drops Claude Code's 1M-context marker from a model id.
function stripOneM(model: string): string {
  return model.replace(/\[1m\]$/i, "");
}

// MANUAL is the slot menu's "type a model id" entry.
const MANUAL = "\u0000manual";

// SlotPicker chooses a slot's model from a list, with a 1M-context switch.
// Typing an id stays possible for a gateway without /models.
function SlotPicker({ value, options, onChange }: { value: string; options: string[]; onChange: (value: string) => void }) {
  const { t } = useI18n();
  const base = stripOneM(value);
  const oneM = base !== value;
  const [manual, setManual] = useState(false);
  const compose = (model: string, wide: boolean) => (model ? model + (wide ? "[1m]" : "") : "");
  const typing = manual || !options.length;
  return (
    <div className="flex min-w-0 items-center gap-1.5">
      {typing ? (
        <input
          value={base}
          onChange={(event) => onChange(compose(event.target.value.trim(), oneM))}
          placeholder={t("modelIdPlaceholder")}
          autoComplete="off"
          spellCheck={false}
          className={control + " font-mono text-[13px]"}
        />
      ) : (
        <select
          value={base}
          onChange={(event) => (event.target.value === MANUAL ? setManual(true) : onChange(compose(event.target.value, oneM)))}
          className={control}
        >
          <option value="">{t("modelNone")}</option>
          {options.map((option) => (
            <option key={option} value={option}>{option}</option>
          ))}
          <option value={MANUAL}>{t("manualInput")}</option>
        </select>
      )}
      {typing && options.length > 0 && (
        <button type="button" title={t("backToList")} onClick={() => setManual(false)} className="shrink-0 rounded-md p-1 text-ink-3 hover:bg-hover hover:text-ink">
          <List size={14} />
        </button>
      )}
      <label title={t("oneMHint")} className={"flex shrink-0 items-center gap-1 text-[11.5px] " + (base ? "text-ink-2" : "text-ink-3 opacity-50")}>
        <input type="checkbox" disabled={!base} checked={oneM} onChange={(event) => onChange(compose(base, event.target.checked))} className="accent-[var(--clay)]" />
        1M
      </label>
    </div>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="grid grid-cols-[150px_1fr] items-center gap-3 text-sm max-sm:grid-cols-1 max-sm:gap-1">
      <span className="text-ink-2">
        {label}
        {hint && <span className="block text-[11.5px] text-ink-3">{hint}</span>}
      </span>
      <span className="flex min-w-0">{children}</span>
    </label>
  );
}

const control =
  "w-full rounded-lg border border-line bg-surface px-3 py-1.5 text-[14px] text-ink outline-none focus:border-line-strong disabled:opacity-60 [&>option]:bg-surface";

function Input({
  value,
  onChange,
  ...props
}: { value: string; onChange: (value: string) => void } & Omit<React.InputHTMLAttributes<HTMLInputElement>, "value" | "onChange">) {
  return <input {...props} value={value} onChange={(event) => onChange(event.target.value)} autoComplete="off" className={control} />;
}

function Select({ value, onChange, options, labels = {} }: {
  value: string;
  onChange: (value: string) => void;
  options: readonly string[];
  labels?: Record<string, string>;
}) {
  return (
    <select value={value} onChange={(event) => onChange(event.target.value)} className={control}>
      {options.map((option) => (
        <option key={option} value={option}>{labels[option] ?? option}</option>
      ))}
    </select>
  );
}

function Button({ children, onClick, primary, type = "button", disabled }: {
  children: ReactNode;
  onClick?: () => void;
  primary?: boolean;
  type?: "button" | "submit";
  disabled?: boolean;
}) {
  return (
    <button
      type={type}
      onClick={onClick}
      disabled={disabled}
      className={
        "flex items-center gap-1.5 rounded-lg border px-3.5 py-1.5 text-sm font-medium disabled:opacity-50 " +
        (primary ? "border-clay bg-clay text-white hover:bg-clay-hover" : "border-line text-ink hover:bg-hover")
      }
    >
      {children}
    </button>
  );
}

function Status({ text }: { text: string }) {
  if (!text) return null;
  return <p className="text-xs text-ink-2">{text}</p>;
}
