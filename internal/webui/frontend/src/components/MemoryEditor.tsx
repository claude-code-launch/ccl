import { useEffect, useRef, useState } from "react";
import { FileText, LoaderCircle, RotateCcw } from "lucide-react";
import { HttpError } from "../api";
import type { Api } from "../api";
import { fileSize } from "../format";
import { useI18n } from "../i18n";

// A file the editor holds: hash is empty for one that does not exist yet.
export interface Loaded {
  path: string;
  hash: string;
  content: string;
  size: number;
  tooBig?: boolean;
}

// MemorySource is one instruction file the editor can switch to.
export interface MemorySource {
  key: string;
  label: string;
  hint: string;
  load: () => Promise<Loaded>;
  // Saves over the loaded file and returns the new hash; a file changed on
  // disk meanwhile is an HttpError 409.
  save: (file: Loaded, content: string) => Promise<string>;
}

export async function readOptional(api: Api, path: string): Promise<Loaded | null> {
  try {
    const file = await api.readFile(path);
    return { path: file.path, hash: file.hash || "", content: file.content || "", size: file.size, tooBig: file.tooBig || file.binary };
  } catch (error) {
    if (error instanceof HttpError && error.status === 404) return null;
    throw error;
  }
}

const missing = (path: string): Loaded => ({ path, hash: "", content: "", size: 0 });

// projectMemorySources are a project's CLAUDE.md (at the root, else in
// .claude/) and CLAUDE.local.md, read and saved through /api/files.
export function projectMemorySources(api: Api, t: (key: "memoryShared" | "memoryLocal") => string): MemorySource[] {
  const save = async (file: Loaded, content: string) => (await api.writeFile(file.path, content, file.hash)).hash;
  return [
    {
      key: "shared",
      label: "CLAUDE.md",
      hint: t("memoryShared"),
      load: async () => (await readOptional(api, "CLAUDE.md")) || (await readOptional(api, ".claude/CLAUDE.md")) || missing("CLAUDE.md"),
      save,
    },
    {
      key: "local",
      label: "CLAUDE.local.md",
      hint: t("memoryLocal"),
      load: async () => (await readOptional(api, "CLAUDE.local.md")) || missing("CLAUDE.local.md"),
      save,
    },
  ];
}

// MemoryEditor edits CLAUDE.md files. A save never overwrites a file that
// changed after it was loaded; unsaved edits ask before they are dropped.
export function MemoryEditor({ title, sources, registerLeaveGuard, framed = true }: {
  title: string;
  sources: MemorySource[];
  registerLeaveGuard: (guard: (() => boolean) | null) => void;
  // Off inside the settings dialog, which has its own frame.
  framed?: boolean;
}) {
  const { t } = useI18n();
  const [key, setKey] = useState(sources[0].key);
  const source = sources.find((item) => item.key === key) || sources[0];
  const [file, setFile] = useState<Loaded | null>(null);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState("");
  const dirty = !!file && draft !== file.content;

  const load = async () => {
    setFile(null);
    setError("");
    setConflict(false);
    setStatus("");
    try {
      const loaded = await source.load();
      setFile(loaded);
      setDraft(loaded.content);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };
  useEffect(() => {
    void load();
    // The editor remounts for another project, so only the file changes here.
  }, [key]);

  // Leaving the page (or the browser tab) with unsaved edits asks first.
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  useEffect(() => {
    registerLeaveGuard(() => !dirtyRef.current || confirm(t("discardChanges")));
    const onUnload = (event: BeforeUnloadEvent) => {
      if (dirtyRef.current) event.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    return () => {
      registerLeaveGuard(null);
      window.removeEventListener("beforeunload", onUnload);
    };
  }, [registerLeaveGuard, t]);

  const save = async () => {
    if (!file || !dirty || saving) return;
    setSaving(true);
    setError("");
    try {
      const hash = await source.save(file, draft);
      setFile({ ...file, hash, content: draft });
      setStatus(t("memoryApplies"));
    } catch (err) {
      if (err instanceof HttpError && err.status === 409) setConflict(true);
      else setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const choose = (next: string) => {
    if (next === key || (dirty && !confirm(t("discardChanges")))) return;
    setKey(next);
  };

  return (
    <section className={"flex min-w-0 flex-col " + (framed ? "rounded-xl border border-line bg-surface" : "")}>
      <div className={"flex flex-wrap items-center gap-2 py-2.5 " + (framed ? "border-b border-line px-4" : "")}>
        <FileText size={15} className="shrink-0 text-ink-3" />
        <h2 className="mr-auto text-[14px] font-medium text-ink">{title}</h2>
        {sources.length > 1 && (
          <div className="flex rounded-lg bg-hover p-0.5 text-[12.5px]">
            {sources.map((item) => (
              <button
                key={item.key}
                type="button"
                onClick={() => choose(item.key)}
                className={"rounded-md px-2 py-0.5 font-mono " + (key === item.key ? "bg-surface text-ink shadow-sm" : "text-ink-3 hover:text-ink")}
              >
                {item.label}
              </button>
            ))}
          </div>
        )}
      </div>
      <div className={"pt-3 text-[12.5px] leading-relaxed text-ink-3 " + (framed ? "px-4" : "")}>
        {source.hint}
        {file && !file.hash && !file.tooBig && <span className="block pt-1">{t("memoryMissing", { file: source.label })}</span>}
        {file && file.path !== source.label && <span className="block break-all pt-1 font-mono">{file.path}</span>}
      </div>
      <div className={"min-w-0 py-3 " + (framed ? "px-4" : "")}>
        {!file && !error && (
          <div className="grid h-40 place-items-center text-ink-3">
            <LoaderCircle size={16} className="animate-spin" />
          </div>
        )}
        {file?.tooBig && <p className="py-6 text-center text-[13px] text-ink-3">{t("fileTooBig", { size: fileSize(file.size) })}</p>}
        {file && !file.tooBig && (
          <textarea
            value={draft}
            onChange={(event) => {
              setDraft(event.target.value);
              setStatus("");
            }}
            onKeyDown={(event) => {
              if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
                event.preventDefault();
                void save();
              }
            }}
            spellCheck={false}
            placeholder={t("memoryPlaceholder")}
            className="block h-[min(52vh,520px)] min-h-[220px] w-full resize-y rounded-lg border border-line bg-canvas px-3 py-2.5 font-mono text-[12.5px] leading-relaxed text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
          />
        )}
      </div>
      {error && <p className={"mb-3 rounded-lg bg-red-500/10 px-3 py-2 text-[12.5px] text-red-500 " + (framed ? "mx-4" : "")}>{error}</p>}
      {conflict && (
        <div className={"mb-3 flex flex-wrap items-center gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-[12.5px] text-ink-2 " + (framed ? "mx-4" : "")}>
          <span className="mr-auto">{t("memoryConflict")}</span>
          <button type="button" onClick={() => void load()} className="flex items-center gap-1 rounded-md px-2 py-0.5 text-ink hover:bg-hover">
            <RotateCcw size={13} /> {t("reload")}
          </button>
        </div>
      )}
      <div className={"flex items-center gap-2 py-2.5 " + (framed ? "border-t border-line px-4" : "")}>
        <span className="mr-auto truncate text-[12.5px] text-ink-3">{dirty ? t("unsaved") : status}</span>
        {dirty && (
          <button type="button" onClick={() => file && setDraft(file.content)} className="h-8 rounded-lg px-3 text-[13px] text-ink-2 hover:bg-hover hover:text-ink">
            {t("cancel")}
          </button>
        )}
        <button
          type="button"
          disabled={!dirty || saving || conflict}
          onClick={() => void save()}
          className="flex h-8 items-center gap-1.5 rounded-lg bg-clay px-3 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
        >
          {saving && <LoaderCircle size={13} className="animate-spin" />}
          {t("save")}
        </button>
      </div>
    </section>
  );
}
