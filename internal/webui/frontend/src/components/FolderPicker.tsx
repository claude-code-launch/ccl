import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { ArrowUp, Folder, FolderGit2, FolderPlus, House, LoaderCircle, X } from "lucide-react";
import type { Api } from "../api";
import type { DirListing } from "../types";
import { IconButton } from "./Sidebar";
import { useI18n } from "../i18n";

// FolderPicker browses the server's folders, like an "Open Folder" dialog,
// and hands back the one chosen. Files are never listed.
export function FolderPicker({ api, start, create = false, onPick, onClose }: {
  api: Api;
  // The folder to open on; the home directory when empty.
  start?: string;
  // create opens with the new-folder field ("new project").
  create?: boolean;
  onPick: (path: string) => Promise<void>;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [listing, setListing] = useState<DirListing | null>(null);
  const [typed, setTyped] = useState("");
  const [hidden, setHidden] = useState(false);
  const [filter, setFilter] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  // newName is the new-folder field; null hides it.
  const [newName, setNewName] = useState<string | null>(create ? "" : null);

  const open = useCallback(
    async (path: string) => {
      setError("");
      try {
        const next = await api.directories(path, hidden);
        setListing(next);
        setTyped(next.path);
        setFilter("");
      } catch (err) {
        setError(err instanceof Error ? err.message : String(err));
      }
    },
    [api, hidden],
  );

  useEffect(() => {
    void open(listing?.path || start || "");
    // Re-list the same folder when hidden folders are toggled.
  }, [open]);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => event.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const go = (event: FormEvent) => {
    event.preventDefault();
    if (typed.trim()) void open(typed.trim());
  };
  // A new folder is created in the one shown and opened as a project.
  const makeFolder = async (event: FormEvent) => {
    event.preventDefault();
    if (!listing || !newName?.trim()) return;
    setBusy(true);
    setError("");
    try {
      const { path } = await api.createDirectory(listing.path, newName.trim());
      await onPick(path);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  const pick = async () => {
    if (!listing) return;
    setBusy(true);
    setError("");
    try {
      await onPick(listing.path);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const needle = filter.trim().toLowerCase();
  const entries = (listing?.entries || []).filter((entry) => !needle || entry.name.toLowerCase().includes(needle));

  return (
    <div className="fixed inset-0 z-40 grid place-items-center bg-black/50 p-4" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <div role="dialog" aria-label={t("openFolderTitle")} className="flex h-[min(620px,88vh)] w-full max-w-[640px] flex-col overflow-hidden rounded-[16px] border border-line bg-canvas text-ink shadow-2xl">
        <header className="flex h-14 shrink-0 items-center justify-between border-b border-line px-5">
          <h3 className="text-[16px] font-medium">{create ? t("newProjectTitle") : t("openFolderTitle")}</h3>
          <IconButton title={t("close")} onClick={onClose}>
            <X size={18} />
          </IconButton>
        </header>
        <form onSubmit={go} className="flex shrink-0 items-center gap-1.5 border-b border-line px-4 py-2.5">
          <IconButton title={t("parentFolder")} onClick={() => listing?.parent && void open(listing.parent)}>
            <ArrowUp size={16} />
          </IconButton>
          <IconButton title={t("homeFolder")} onClick={() => void open(listing?.home || "")}>
            <House size={16} />
          </IconButton>
          <IconButton title={t("newFolder")} active={newName !== null} onClick={() => setNewName(newName === null ? "" : null)}>
            <FolderPlus size={16} />
          </IconButton>
          <input
            value={typed}
            onChange={(event) => setTyped(event.target.value)}
            spellCheck={false}
            className="min-w-0 flex-1 rounded-lg border border-line bg-surface px-3 py-1.5 font-mono text-[13px] text-ink outline-none focus:border-line-strong"
          />
          <button type="submit" className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-ink hover:bg-hover">
            {t("goToPath")}
          </button>
        </form>
        {newName !== null && (
          <form onSubmit={makeFolder} className="flex shrink-0 items-center gap-2 border-b border-line bg-hover/50 px-4 py-2.5">
            <FolderPlus size={16} className="shrink-0 text-ink-3" />
            <input
              autoFocus
              value={newName}
              onChange={(event) => setNewName(event.target.value)}
              placeholder={t("newFolderName")}
              spellCheck={false}
              className="min-w-0 flex-1 rounded-lg border border-line bg-surface px-3 py-1.5 text-[14px] text-ink outline-none focus:border-line-strong"
            />
            <button
              type="submit"
              disabled={!newName.trim() || busy}
              className="shrink-0 rounded-lg border border-clay bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-50"
            >
              {t("createAndOpen")}
            </button>
          </form>
        )}
        <div className="flex shrink-0 items-center gap-3 px-4 pt-2.5">
          <input
            value={filter}
            onChange={(event) => setFilter(event.target.value)}
            placeholder={t("filterFolders")}
            className="min-w-0 flex-1 rounded-lg border border-line bg-surface px-3 py-1 text-[13px] text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
          />
          <label className="flex shrink-0 items-center gap-1.5 text-[12.5px] text-ink-3">
            <input type="checkbox" checked={hidden} onChange={(event) => setHidden(event.target.checked)} className="accent-[var(--clay)]" />
            {t("showHidden")}
          </label>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto px-2 py-2">
          {!listing && !error && <LoaderCircle size={16} className="m-3 animate-spin text-ink-3" />}
          {listing && !entries.length && <p className="px-3 py-6 text-center text-[13px] text-ink-3">{t("noSubfolders")}</p>}
          {entries.map((entry) => (
            <button
              key={entry.path}
              onClick={() => void open(entry.path)}
              title={entry.path}
              className="flex w-full items-center gap-2.5 rounded-lg px-3 py-1.5 text-left text-[14px] hover:bg-hover"
            >
              {entry.repo ? <FolderGit2 size={16} className="shrink-0 text-clay" /> : <Folder size={16} className="shrink-0 text-ink-3" />}
              <span className="truncate">{entry.name}</span>
            </button>
          ))}
        </div>
        <footer className="shrink-0 space-y-2 border-t border-line px-5 py-3">
          {error && <p className="text-xs text-red-500">{error}</p>}
          <div className="flex items-center gap-3">
            <p className="min-w-0 flex-1 text-[12px] text-ink-3">{t("folderHint")}</p>
            <button onClick={onClose} className="rounded-lg border border-line px-3.5 py-1.5 text-sm text-ink hover:bg-hover">
              {t("cancel")}
            </button>
            <button
              onClick={pick}
              disabled={!listing || busy}
              className="flex items-center gap-1.5 rounded-lg border border-clay bg-clay px-3.5 py-1.5 text-sm font-medium text-white hover:bg-clay-hover disabled:opacity-50"
            >
              {listing?.repo ? <FolderGit2 size={14} /> : <Folder size={14} />}
              {t("chooseFolder")}
            </button>
          </div>
        </footer>
      </div>
    </div>
  );
}
