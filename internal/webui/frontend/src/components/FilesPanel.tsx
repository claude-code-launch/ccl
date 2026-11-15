import { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";
import { ArrowLeft, AtSign, ChevronRight, File, FilePlus, Folder, FolderOpen, LoaderCircle, RefreshCw, Save, Upload } from "lucide-react";
import type { Api } from "../api";
import type { FileContent, FileEntry } from "../types";
import { fileSize } from "../format";
import { useI18n } from "../i18n";
import { IconButton } from "./Sidebar";

const CodeEditor = lazy(() => import("./CodeEditor"));

interface Open {
  file: FileContent;
  // What the editor holds and what was last loaded or saved.
  content: string;
  saved: string;
  hash: string;
  conflict?: string;
  saving?: boolean;
  isNew?: boolean;
}

interface Props {
  api: Api;
  // Bumped when a turn ends: Claude may have changed the tree.
  refreshKey: number;
  onMention: (path: string) => void;
  onWide: (wide: boolean) => void;
  onError: (error: unknown) => void;
  // A save changes the working tree the git status describes.
  onSaved: () => void;
}

// The workspace browser: a lazily expanded tree, an editor that refuses to
// overwrite a file Claude changed meanwhile, image preview and uploads.
export function FilesPanel({ api, refreshKey, onMention, onWide, onError, onSaved }: Props) {
  const { t } = useI18n();
  const [dirs, setDirs] = useState<Record<string, FileEntry[]>>({});
  const [expanded, setExpanded] = useState<Set<string>>(new Set([""]));
  const [target, setTarget] = useState("");
  const [open, setOpen] = useState<Open | null>(null);
  const [loading, setLoading] = useState("");
  const [dragging, setDragging] = useState(false);
  const picker = useRef<HTMLInputElement>(null);

  const loadDir = useCallback(
    async (path: string) => {
      try {
        const listing = await api.listFiles(path);
        setDirs((all) => ({ ...all, [path]: listing.entries }));
      } catch (error) {
        onError(error);
      }
    },
    [api, onError],
  );

  // Reload every expanded directory when Claude may have changed files.
  useEffect(() => {
    for (const path of expanded) void loadDir(path);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshKey, loadDir]);

  useEffect(() => onWide(!!open), [open, onWide]);

  const toggle = (entry: FileEntry) => {
    setTarget(entry.path);
    setExpanded((set) => {
      const next = new Set(set);
      if (next.has(entry.path)) next.delete(entry.path);
      else {
        next.add(entry.path);
        void loadDir(entry.path);
      }
      return next;
    });
  };

  const openFile = async (path: string) => {
    setLoading(path);
    try {
      const file = await api.readFile(path);
      setOpen({ file, content: file.content || "", saved: file.content || "", hash: file.hash });
    } catch (error) {
      onError(error);
    } finally {
      setLoading("");
    }
  };

  const save = async (overwrite = false) => {
    if (!open || open.saving || (open.content === open.saved && !open.isNew)) return;
    const hash = overwrite && open.conflict ? open.conflict : open.hash;
    setOpen({ ...open, saving: true });
    try {
      const result = await api.writeFile(open.file.path, open.content, hash);
      setOpen((current) => current && { ...current, saved: current.content, hash: result.hash, conflict: undefined, saving: false, isNew: false });
      void loadDir(parentOf(open.file.path));
      onSaved();
    } catch (error) {
      // A 409 carries the hash on disk; saving with it overwrites on purpose.
      const message = error instanceof Error ? error.message : String(error);
      if (/changed on disk|deleted since/.test(message)) {
        const latest = await api.readFile(open.file.path).catch(() => null);
        setOpen((current) => current && { ...current, saving: false, conflict: latest?.hash || "" });
      } else {
        setOpen((current) => current && { ...current, saving: false });
        onError(error);
      }
    }
  };

  const reload = async () => {
    if (!open) return;
    const file = await api.readFile(open.file.path).catch((error) => (onError(error), null));
    if (file) setOpen({ file, content: file.content || "", saved: file.content || "", hash: file.hash });
  };

  const newFile = () => {
    const name = prompt(t("newFilePrompt"), target && dirs[target] ? target + "/" : "");
    if (!name || !name.trim()) return;
    const path = name.trim().replace(/^\/+/, "");
    setOpen({
      file: { path, size: 0, modified: "", hash: "", type: "text/plain", binary: false, image: false },
      content: "",
      saved: "",
      hash: "",
      isNew: true,
    });
  };

  const upload = async (files: File[]) => {
    if (!files.length) return;
    const dir = dirs[target] ? target : parentOf(target);
    try {
      await api.uploadToWorkspace(dir, files);
      onSaved();
      setExpanded((set) => new Set(set).add(dir));
      void loadDir(dir);
    } catch (error) {
      onError(error);
    }
  };

  if (open) {
    const dirty = open.content !== open.saved || open.isNew;
    return (
      <div className="flex h-full min-h-0 flex-col">
        <div className="flex items-center gap-1 border-b border-line px-1.5 py-1">
          <IconButton title={t("back")} onClick={() => (!dirty || confirm(t("discardChanges"))) && setOpen(null)}>
            <ArrowLeft size={16} />
          </IconButton>
          <span className="min-w-0 flex-1 truncate text-[13px] text-ink" title={open.file.path}>
            {open.file.path}
            {dirty && <span className="ml-1.5 text-clay">●</span>}
          </span>
          <IconButton title={t("mention")} onClick={() => onMention(open.file.path)}>
            <AtSign size={15} />
          </IconButton>
          {!open.file.binary && !open.file.tooBig && (
            <button
              onClick={() => save()}
              disabled={!dirty || open.saving}
              className="flex items-center gap-1.5 rounded-lg bg-clay px-2.5 py-1 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
            >
              {open.saving ? <LoaderCircle size={13} className="animate-spin" /> : <Save size={13} />}
              {t("save")}
            </button>
          )}
        </div>
        {open.conflict !== undefined && (
          <div className="flex flex-wrap items-center gap-2 border-b border-line bg-amber-500/10 px-3 py-2 text-[13px] text-ink">
            {t("fileConflict")}
            <button onClick={reload} className="rounded-md border border-line px-2 py-0.5 hover:bg-hover">{t("reload")}</button>
            <button onClick={() => save(true)} className="rounded-md border border-line px-2 py-0.5 text-red-500 hover:bg-hover">{t("overwrite")}</button>
          </div>
        )}
        <div className="min-h-0 flex-1">
          {open.file.image ? (
            <div className="grid h-full place-items-center overflow-auto p-4">
              <img src={api.rawFileURL(open.file.path)} alt={open.file.path} className="max-h-full max-w-full rounded-lg border border-line" />
            </div>
          ) : open.file.binary || open.file.tooBig ? (
            <div className="p-4 text-[13px] text-ink-3">
              {open.file.tooBig ? t("fileTooBig", { size: fileSize(open.file.size) }) : t("binaryFile")}{" "}
              <a href={api.rawFileURL(open.file.path)} className="text-clay underline">{t("download")}</a>
            </div>
          ) : (
            <Suspense fallback={<div className="p-4"><LoaderCircle size={16} className="animate-spin text-ink-3" /></div>}>
              <CodeEditor
                value={open.saved === open.content ? open.saved : open.content}
                filename={open.file.path}
                onChange={(content) => setOpen((current) => current && { ...current, content })}
                onSave={() => void save()}
              />
            </Suspense>
          )}
        </div>
      </div>
    );
  }

  const renderDir = (path: string, depth: number) =>
    (dirs[path] || []).map((entry) => {
      const isDir = entry.type === "dir";
      const isOpen = expanded.has(entry.path);
      return (
        <div key={entry.path}>
          <div
            onClick={() => (isDir ? toggle(entry) : (setTarget(entry.path), void openFile(entry.path)))}
            title={entry.path}
            style={{ paddingLeft: 6 + depth * 14 }}
            className={"group/file flex h-7 cursor-pointer items-center gap-1.5 rounded-md pr-1 text-[13px] hover:bg-hover " + (target === entry.path ? "bg-active text-ink" : "text-ink-2")}
          >
            {isDir ? (
              <>
                <ChevronRight size={13} className={"shrink-0 text-ink-3 transition " + (isOpen ? "rotate-90" : "")} />
                {isOpen ? <FolderOpen size={14} className="shrink-0 text-ink-3" /> : <Folder size={14} className="shrink-0 text-ink-3" />}
              </>
            ) : (
              <>
                <span className="w-[13px] shrink-0" />
                {loading === entry.path ? <LoaderCircle size={14} className="shrink-0 animate-spin text-ink-3" /> : <File size={14} className="shrink-0 text-ink-3" />}
              </>
            )}
            <span className="min-w-0 flex-1 truncate">{entry.name}</span>
            <button
              onClick={(event) => {
                event.stopPropagation();
                onMention(entry.path);
              }}
              title={t("mention")}
              className="hidden size-5 shrink-0 place-items-center rounded text-ink-3 hover:bg-active hover:text-ink group-hover/file:grid"
            >
              <AtSign size={12} />
            </button>
          </div>
          {isDir && isOpen && renderDir(entry.path, depth + 1)}
        </div>
      );
    });

  return (
    <div
      className={"relative flex h-full min-h-0 flex-col " + (dragging ? "ring-2 ring-inset ring-clay/60" : "")}
      onDragOver={(event) => {
        if (Array.from(event.dataTransfer.types).includes("Files")) {
          event.preventDefault();
          event.stopPropagation();
          setDragging(true);
        }
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(event) => {
        if (!Array.from(event.dataTransfer.types).includes("Files")) return;
        event.preventDefault();
        event.stopPropagation();
        setDragging(false);
        void upload(Array.from(event.dataTransfer.files));
      }}
    >
      <div className="flex items-center gap-0.5 px-1.5 py-1">
        <span className="flex-1 px-1 text-[13px] text-ink-2">{t("files")}</span>
        <IconButton title={t("newFile")} onClick={newFile}><FilePlus size={15} /></IconButton>
        <IconButton title={t("uploadHere")} onClick={() => picker.current?.click()}><Upload size={15} /></IconButton>
        <IconButton title={t("refresh")} onClick={() => expanded.forEach((path) => void loadDir(path))}><RefreshCw size={15} /></IconButton>
        <input ref={picker} type="file" multiple hidden onChange={(event) => { void upload(Array.from(event.target.files || [])); event.target.value = ""; }} />
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-3">
        {!dirs[""] && <LoaderCircle size={14} className="m-3 animate-spin text-ink-3" />}
        {dirs[""] && !dirs[""].length && <p className="px-2 py-2 text-[13px] text-ink-3">{t("emptyDir")}</p>}
        {renderDir("", 0)}
      </div>
    </div>
  );
}

function parentOf(path: string): string {
  const index = path.lastIndexOf("/");
  return index > 0 ? path.slice(0, index) : "";
}
