import { useEffect, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { X } from "lucide-react";
import type { Api } from "../api";
import type { ConfigView, ProviderView, TestResult } from "../types";
import { EFFORT_LEVELS } from "../types";
import { IconButton } from "./Sidebar";
import { useI18n } from "../i18n";
import { MCPServers, SubscriptionLogin } from "./SettingsExtras";

// The drawer is the page's only writer of ~/.ccl/config.yaml, and every write
// goes through the server's config.Update.

const PROVIDER_TYPES = ["anthropic", "openai", "openai_responses", "modelsdev", "autoclaw"];
const SLOT_FIELDS = [
  ["opusModel", "opus"],
  ["sonnetModel", "sonnet"],
  ["haikuModel", "haiku"],
  ["fableModel", "fable"],
  ["customModelId", "custom"],
  ["subagentModel", "subagent"],
] as const;

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
  config: ConfigView | null;
  loadError: string;
  onClose: () => void;
  onChanged: () => Promise<void>;
}

export function SettingsDrawer({ api, config, loadError, onClose, onChanged }: Props) {
  const { t } = useI18n();
  const [active, setActive] = useState("default");
  const [acp, setAcp] = useState("default");
  const [lang, setLang] = useState("");
  const [bypass, setBypass] = useState(false);
  const [globalStatus, setGlobalStatus] = useState("");
  const [form, setForm] = useState<Form>(emptyForm);
  const [editing, setEditing] = useState<ProviderView | null>(null);
  const [formStatus, setFormStatus] = useState("");

  useEffect(() => {
    if (!config) return;
    setActive(config.active?.normal || "default");
    setAcp(config.active?.acp || "default");
    setLang(config.lang || "");
    setBypass(!!config.bypassMode);
  }, [config]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => event.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const providers = Object.values(config?.providers || {}).sort((a, b) => a.name.localeCompare(b.name));
  const names = ["default", ...providers.map((provider) => provider.name)];

  const run = async (status: (message: string) => void, action: () => Promise<string>) => {
    try {
      status(await action());
    } catch (error) {
      status(error instanceof Error ? error.message : String(error));
    }
  };

  const saveGlobal = () =>
    run(setGlobalStatus, async () => {
      await api.putConfig({ active: { normal: active, acp }, lang, bypassMode: bypass });
      await onChanged();
      return t("saved");
    });

  const use = (name: string) =>
    run(setGlobalStatus, async () => {
      await api.putConfig({ active: { normal: name, acp: name } });
      await onChanged();
      // Conversations keep their own provider; new ones start with this one.
      return t("nowActive", { name });
    });

  const remove = (name: string) => {
    if (!confirm(t("confirmDeleteProvider", { name }))) return;
    run(setGlobalStatus, async () => {
      await api.deleteProvider(name);
      await onChanged();
      if (editing?.name === name) reset();
      return t("providerDeleted", { name });
    });
  };

  const describe = (result: TestResult) =>
    result.ok
      ? t("testOk", { ms: result.elapsedMs }) + (result.models?.length ? " · " + result.models.slice(0, 8).join(", ") : "")
      : t("testFailed", { message: result.message || "unknown error" });

  const test = (name: string) => {
    setFormStatus(t("testing", { name }));
    run(setFormStatus, async () => describe(await api.testProvider(name)));
  };

  const edit = (provider: ProviderView) => {
    setEditing(provider);
    const next = emptyForm();
    next.name = provider.name;
    next.type = provider.type || "anthropic";
    next.endpoint = provider.endpoint || "";
    next.effortLevel = provider.effortLevel || "";
    for (const [field] of SLOT_FIELDS) next[field] = (provider[field] as string) || "";
    setForm(next);
    setFormStatus("");
  };

  const reset = () => {
    setEditing(null);
    setForm(emptyForm());
    setFormStatus("");
  };

  const testForm = () => {
    if (!form.name.trim()) {
      setFormStatus(t("saveFirst"));
      return;
    }
    setFormStatus(t("testing", { name: form.name }));
    run(setFormStatus, async () =>
      describe(
        await api.testProvider(form.name.trim(), {
          endpoint: form.endpoint.trim() || undefined,
          apiKey: form.apiKey.trim() || undefined,
          type: form.type || undefined,
        }),
      ),
    );
  };

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const name = form.name.trim();
    if (!name) return;
    const body: Record<string, unknown> = { name, type: form.type, endpoint: form.endpoint.trim(), effortLevel: form.effortLevel };
    for (const [field] of SLOT_FIELDS) body[field] = form[field].trim();
    // An empty key field means "keep the stored key".
    if (form.apiKey.trim()) body.apiKey = form.apiKey.trim();
    run(setFormStatus, async () => {
      if (editing) await api.patchProvider(name, body);
      else await api.createProvider(body);
      await onChanged();
      if (!editing) setForm(emptyForm());
      return t("providerSaved", { name });
    });
  };

  const set = (field: string) => (value: string) => setForm((current) => ({ ...current, [field]: value }));

  return (
    <div className="fixed inset-0 z-30">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} />
      <div
        role="dialog"
        aria-label="Settings"
        className="absolute inset-y-0 right-0 flex w-full max-w-[640px] flex-col border-l border-line bg-canvas text-ink shadow-2xl"
      >
        <header className="flex h-12 items-center justify-between border-b border-line px-5">
          <h2 className="font-display text-[18px] font-medium">{t("settings")}</h2>
          <IconButton title={t("close")} onClick={onClose}>
            <X size={18} />
          </IconButton>
        </header>
        <div className="flex-1 space-y-8 overflow-y-auto px-6 py-5">
          {loadError && <p className="rounded-lg bg-red-500/10 px-3 py-2 text-[13px] text-red-500">{t("couldNotLoad", { error: loadError })}</p>}

          <Section title="ccl">
            <Field label={t("activeProvider")}>
              <Select value={active} onChange={setActive} options={names} />
            </Field>
            <Field label={t("acpProvider")}>
              <Select value={acp} onChange={setAcp} options={names} />
            </Field>
            <Field label={t("language")}>
              <Select value={lang} onChange={setLang} options={["", "zh", "en"]} labels={{ "": t("langAuto"), zh: "中文", en: "English" }} />
            </Field>
            <Field label={t("bypass")}>
              <input type="checkbox" checked={bypass} onChange={(event) => setBypass(event.target.checked)} className="size-4 accent-[var(--clay)]" />
            </Field>
            <div className="flex items-center gap-3">
              <Button primary onClick={saveGlobal}>{t("save")}</Button>
              <Status text={globalStatus} />
            </div>
          </Section>

          <Section title={t("providers")}>
            <div className="overflow-x-auto rounded-[10px] border border-line">
              <table className="w-full text-left text-[13px]">
                <thead className="bg-hover text-ink-3">
                  <tr>
                    {[t("colName"), t("colType"), t("colEndpoint"), t("colKey"), t("colModels"), ""].map((title) => (
                      <th key={title} className="whitespace-nowrap px-3 py-2 font-medium">{title}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {!providers.length && (
                    <tr>
                      <td colSpan={6} className="px-3 py-3 text-ink-3">{t("noProviders")}</td>
                    </tr>
                  )}
                  {providers.map((provider) => (
                    <tr key={provider.name} className="border-t border-line">
                      <td className="whitespace-nowrap px-3 py-2 font-medium">
                        {provider.name}
                        {provider.active && <span className="ml-1.5 rounded bg-clay/15 px-1.5 text-[11px] text-clay">{t("active")}</span>}
                      </td>
                      <td className="whitespace-nowrap px-3 py-2">{provider.type || "—"}</td>
                      <td className="max-w-[160px] truncate px-3 py-2" title={provider.endpoint}>{provider.endpoint || "—"}</td>
                      <td className="whitespace-nowrap px-3 py-2 font-mono">{provider.hasKey ? provider.apiKey : "(none)"}</td>
                      <td className="whitespace-nowrap px-3 py-2">{provider.modelChoices?.length || 0}</td>
                      <td className="whitespace-nowrap px-3 py-2">
                        <div className="flex gap-1">
                          <Button small onClick={() => edit(provider)}>{t("edit")}</Button>
                          <Button small onClick={() => test(provider.name)}>{t("test")}</Button>
                          <Button small onClick={() => use(provider.name)}>{t("use")}</Button>
                          <Button small danger onClick={() => remove(provider.name)}>{t("delete")}</Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="text-xs text-ink-3">{t("defaultNote")}</p>
          </Section>

          <Section title={t("subscriptionLogin")}>
            <SubscriptionLogin api={api} onDone={() => void onChanged()} />
          </Section>

          <Section title={t("connectorsTitle")}>
            <MCPServers api={api} />
          </Section>

          <Section title={editing ? t("editProvider", { name: editing.name }) : t("newProvider")}>
            <form onSubmit={submit} className="space-y-2">
              <Field label={t("name")}>
                <Input value={form.name} onChange={set("name")} disabled={!!editing} required />
              </Field>
              <Field label={t("type")}>
                <Select value={form.type} onChange={set("type")} options={PROVIDER_TYPES} />
              </Field>
              <Field label={t("endpoint")}>
                <Input value={form.endpoint} onChange={set("endpoint")} placeholder="https://…" />
              </Field>
              <Field label={t("apiKey")}>
                <Input
                  type="password"
                  value={form.apiKey}
                  onChange={set("apiKey")}
                  placeholder={editing?.hasKey ? t("keepKey") : t("apiKey")}
                />
              </Field>
              {SLOT_FIELDS.map(([field, label]) => (
                <Field key={field} label={label}>
                  <Input value={form[field]} onChange={set(field)} />
                </Field>
              ))}
              <Field label={t("effort")}>
                <Select value={form.effortLevel} onChange={set("effortLevel")} options={["", ...EFFORT_LEVELS]} labels={{ "": t("effortDefault") }} />
              </Field>
              <div className="flex flex-wrap items-center gap-2 pt-2">
                <Button primary type="submit">{t("saveProvider")}</Button>
                <Button onClick={reset}>{t("clear")}</Button>
                <Button onClick={testForm}>{t("test")}</Button>
              </div>
              <Status text={formStatus} />
            </form>
          </Section>
        </div>
      </div>
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="space-y-3">
      <h3 className="text-[13px] font-medium text-ink-2">{title}</h3>
      {children}
    </section>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="grid grid-cols-[130px_1fr] items-center gap-3 text-sm">
      <span className="text-ink-3">{label}</span>
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

function Select({
  value,
  onChange,
  options,
  labels = {},
}: {
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

function Button({
  children,
  onClick,
  primary,
  danger,
  small,
  type = "button",
}: {
  children: ReactNode;
  onClick?: () => void;
  primary?: boolean;
  danger?: boolean;
  small?: boolean;
  type?: "button" | "submit";
}) {
  const tone = primary
    ? "border-clay bg-clay text-white hover:bg-clay-hover"
    : danger
      ? "border-line text-red-500 hover:bg-red-500/10"
      : "border-line text-ink hover:bg-hover";
  return (
    <button type={type} onClick={onClick} className={`rounded-lg border font-medium ${small ? "px-2 py-0.5 text-xs" : "px-3.5 py-1.5 text-sm"} ${tone}`}>
      {children}
    </button>
  );
}

function Status({ text }: { text: string }) {
  if (!text) return null;
  return <p className="text-xs text-ink-3">{text}</p>;
}
