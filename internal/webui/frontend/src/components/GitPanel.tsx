import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { ArrowLeft, Check, GitBranch, GitCommitHorizontal, LoaderCircle, Minus, Plus, RefreshCw } from "lucide-react";
import type { Api } from "../api";
import type { GitBranch as Branch, GitCommit, GitFile, GitStatus } from "../types";
import { relativeTime } from "../format";
import { useI18n } from "../i18n";
import { IconButton } from "./Sidebar";
import { DiffStat } from "./ToolViews";
import { UnifiedDiff } from "./UnifiedDiff";

type Tab = "changes" | "history" | "branches";
type Viewing = { title: string; diff: string; truncated: boolean } | null;

interface Props {
  api: Api;
  status: GitStatus | null;
  onStatus: (status: GitStatus) => void;
  onRefresh: () => void;
  // A file the overview asked to show.
  focus: string;
  onWide: (wide: boolean) => void;
  onError: (error: unknown) => void;
}

const staged = (file: GitFile) => !file.untracked && file.index !== "." && file.index !== "?";
const unstaged = (file: GitFile) => file.untracked || file.worktree !== ".";

export function GitPanel({ api, status, onStatus, onRefresh, focus, onWide, onError }: Props) {
  const { t, lang } = useI18n();
  const [tab, setTab] = useState<Tab>("changes");
  const [viewing, setViewing] = useState<Viewing>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [commits, setCommits] = useState<GitCommit[] | null>(null);
  const [branches, setBranches] = useState<Branch[] | null>(null);
  const [worktrees, setWorktrees] = useState<{ path: string; branch?: string; current?: boolean }[]>([]);

  useEffect(() => onWide(!!viewing), [viewing, onWide]);
  // The status the page holds may predate edits made since: read it afresh.
  useEffect(() => {
    onRefresh();
  }, [onRefresh]);

  const run = async (action: () => Promise<GitStatus>) => {
    setBusy(true);
    try {
      onStatus(await action());
    } catch (error) {
      onError(error);
    } finally {
      setBusy(false);
    }
  };

  const showFile = useCallback(
    async (file: GitFile, fromIndex: boolean) => {
      try {
        const result = await api.gitDiff(file.path, { staged: fromIndex, untracked: file.untracked });
        setViewing({ title: file.path, ...result });
      } catch (error) {
        onError(error);
      }
    },
    [api, onError],
  );

  // The overview's changes card opens a file here.
  useEffect(() => {
    if (!focus || !status) return;
    const file = status.files.find((item) => item.path === focus);
    if (file) {
      setTab("changes");
      void showFile(file, !unstaged(file));
    }
  }, [focus, status, showFile]);

  useEffect(() => {
    if (tab === "history" && commits === null) api.gitLog(100).then((result) => setCommits(result.commits)).catch(onError);
    if (tab === "branches" && branches === null) {
      api.gitBranches().then((result) => setBranches(result.branches)).catch(onError);
      api.gitWorktrees().then((result) => setWorktrees(result.worktrees)).catch(() => setWorktrees([]));
    }
  }, [tab, commits, branches, api, onError]);

  if (viewing) {
    return (
      <div className="flex h-full min-h-0 flex-col">
        <div className="flex items-center gap-1 border-b border-line px-1.5 py-1">
          <IconButton title={t("back")} onClick={() => setViewing(null)}><ArrowLeft size={16} /></IconButton>
          <span className="min-w-0 flex-1 truncate text-[13px] text-ink" title={viewing.title}>{viewing.title}</span>
        </div>
        <div className="min-h-0 flex-1 overflow-auto pb-3">
          {viewing.diff ? <UnifiedDiff diff={viewing.diff} /> : <p className="p-3 text-[13px] text-ink-3">{t("noDiff")}</p>}
          {viewing.truncated && <p className="px-3 pt-2 text-xs text-ink-3">{t("diffTruncated")}</p>}
        </div>
      </div>
    );
  }

  if (!status) return <LoaderCircle size={14} className="m-3 animate-spin text-ink-3" />;
  if (!status.repo) return <p className="p-3 text-[13px] text-ink-3">{status.error || t("notARepo")}</p>;

  const stagedFiles = status.files.filter(staged);
  const changedFiles = status.files.filter(unstaged);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-1 px-1.5 py-1">
        <span className="flex min-w-0 flex-1 items-center gap-1.5 px-1 text-[13px] text-ink">
          <GitBranch size={14} className="shrink-0 text-ink-3" />
          <span className="truncate">{status.detached ? t("detached") : status.branch}</span>
          {status.ahead > 0 && <span className="text-xs text-ink-3">↑{status.ahead}</span>}
          {status.behind > 0 && <span className="text-xs text-ink-3">↓{status.behind}</span>}
        </span>
        {busy && <LoaderCircle size={14} className="animate-spin text-ink-3" />}
        <IconButton title={t("refresh")} onClick={() => { onRefresh(); setCommits(null); setBranches(null); }}><RefreshCw size={15} /></IconButton>
      </div>
      <div className="mx-2 mb-1 flex gap-0.5 rounded-lg bg-hover p-0.5 text-[13px]">
        {(["changes", "history", "branches"] as Tab[]).map((name) => (
          <button key={name} onClick={() => setTab(name)} className={"flex-1 rounded-md py-1 " + (tab === name ? "bg-surface text-ink shadow-sm" : "text-ink-2 hover:text-ink")}>
            {t(name === "changes" ? "changes" : name === "history" ? "history" : "branches")}
            {name === "changes" && status.files.length > 0 && <span className="ml-1 text-xs text-ink-3">{status.files.length}</span>}
          </button>
        ))}
      </div>

      {tab === "changes" && (
        <>
          <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-2">
            {!status.files.length && <p className="px-2 py-3 text-[13px] text-ink-3">{t("noChanges")}</p>}
            {stagedFiles.length > 0 && (
              <Group title={t("staged")} action={<SmallButton onClick={() => run(() => api.gitUnstage(stagedFiles.map((f) => f.path)))}>{t("unstageAll")}</SmallButton>}>
                {stagedFiles.map((file) => (
                  <FileRow key={"s" + file.path} file={file} letter={file.index} onOpen={() => showFile(file, true)}
                    action={<RowButton title={t("unstage")} onClick={() => run(() => api.gitUnstage([file.path]))}><Minus size={13} /></RowButton>} />
                ))}
              </Group>
            )}
            {changedFiles.length > 0 && (
              <Group title={t("unstaged")} action={<SmallButton onClick={() => run(() => api.gitStage(changedFiles.map((f) => f.path)))}>{t("stageAll")}</SmallButton>}>
                {changedFiles.map((file) => (
                  <FileRow key={"c" + file.path} file={file} letter={file.untracked ? "U" : file.worktree} onOpen={() => showFile(file, false)}
                    action={<RowButton title={t("stage")} onClick={() => run(() => api.gitStage([file.path]))}><Plus size={13} /></RowButton>} />
                ))}
              </Group>
            )}
          </div>
          <div className="border-t border-line p-2">
            <textarea
              value={message}
              onChange={(event) => setMessage(event.target.value)}
              placeholder={t("commitMessage")}
              rows={2}
              className="block w-full resize-none rounded-lg border border-line bg-surface px-2.5 py-1.5 text-[13px] text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
            />
            <button
              disabled={busy || !message.trim() || !stagedFiles.length}
              onClick={() => run(async () => {
                const result = await api.gitCommit(message);
                setMessage("");
                setCommits(null);
                return result;
              })}
              className="mt-1.5 flex w-full items-center justify-center gap-1.5 rounded-lg bg-clay py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
            >
              <Check size={14} /> {t("commitN", { n: stagedFiles.length })}
            </button>
          </div>
        </>
      )}

      {tab === "history" && (
        <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-2">
          {commits === null && <LoaderCircle size={14} className="m-3 animate-spin text-ink-3" />}
          {commits?.map((commit) => (
            <button
              key={commit.hash}
              onClick={async () => {
                try {
                  setViewing({ title: `${commit.short} ${commit.subject}`, ...(await api.gitShow(commit.hash)) });
                } catch (error) {
                  onError(error);
                }
              }}
              className="flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-hover"
            >
              <GitCommitHorizontal size={14} className="mt-0.5 shrink-0 text-ink-3" />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[13px] text-ink">{commit.subject}</span>
                <span className="block truncate text-xs text-ink-3">{commit.short} · {commit.author} · {relativeTime(commit.date, lang)}</span>
              </span>
            </button>
          ))}
        </div>
      )}

      {tab === "branches" && (
        <div className="min-h-0 flex-1 overflow-y-auto px-1 pb-2">
          <button
            onClick={() => {
              const name = prompt(t("newBranchPrompt"));
              if (name?.trim()) void run(async () => {
                const result = await api.gitCheckout(name.trim(), true);
                setBranches(null);
                return result;
              });
            }}
            className="mb-1 flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[13px] text-ink-2 hover:bg-hover hover:text-ink"
          >
            <Plus size={14} /> {t("newBranch")}
          </button>
          {branches === null && <LoaderCircle size={14} className="m-3 animate-spin text-ink-3" />}
          {branches?.map((branch) => (
            <button
              key={branch.name}
              disabled={branch.current || busy}
              onClick={() => confirm(t("switchBranch", { name: branch.name })) && void run(async () => {
                const result = await api.gitCheckout(branch.name);
                setBranches(null);
                setCommits(null);
                return result;
              })}
              className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[13px] enabled:hover:bg-hover"
            >
              {branch.current ? <Check size={14} className="shrink-0 text-clay" /> : <GitBranch size={14} className="shrink-0 text-ink-3" />}
              <span className={"min-w-0 flex-1 truncate " + (branch.current ? "text-ink" : "text-ink-2")}>{branch.name}</span>
              {branch.date && <span className="shrink-0 text-xs text-ink-3">{relativeTime(branch.date, lang)}</span>}
            </button>
          ))}
          {worktrees.length > 1 && (
            <Group title={t("worktrees")}>
              {worktrees.map((tree) => (
                <div key={tree.path} title={tree.path} className="flex items-center gap-2 px-2 py-1 text-[13px]">
                  <span className={"size-1.5 shrink-0 rounded-full " + (tree.current ? "bg-clay" : "bg-ink-3")} />
                  <span className="min-w-0 flex-1 truncate text-ink-2">{tree.path}</span>
                  <span className="shrink-0 text-xs text-ink-3">{tree.branch || "detached"}</span>
                </div>
              ))}
            </Group>
          )}
        </div>
      )}
    </div>
  );
}

function Group({ title, action, children }: { title: string; action?: ReactNode; children: ReactNode }) {
  return (
    <div className="mt-1.5">
      <div className="flex items-center px-2 pb-0.5 text-xs text-ink-3">
        <span className="flex-1">{title}</span>
        {action}
      </div>
      {children}
    </div>
  );
}

function FileRow({ file, letter, onOpen, action }: { file: GitFile; letter: string; onOpen: () => void; action: ReactNode }) {
  const slash = file.path.lastIndexOf("/");
  const name = file.path.slice(slash + 1);
  const dir = slash > 0 ? file.path.slice(0, slash) : "";
  return (
    <div onClick={onOpen} title={file.path} className="group/row flex h-7 cursor-pointer items-center gap-2 rounded-md pl-2 pr-1 text-[13px] hover:bg-hover">
      <span className={"w-3 shrink-0 font-mono text-[11px] " + (file.untracked || letter === "A" ? "text-emerald-500" : letter === "D" ? "text-red-500" : file.conflicted ? "text-red-500" : "text-amber-500")}>{letter}</span>
      <span className="min-w-0 flex-1 truncate text-ink">
        {name}
        {dir && <span className="ml-1.5 text-xs text-ink-3">{dir}</span>}
      </span>
      <DiffStat added={file.added} deleted={file.deleted} />
      <span className="hidden group-hover/row:block" onClick={(event) => event.stopPropagation()}>{action}</span>
    </div>
  );
}

function RowButton({ title, onClick, children }: { title: string; onClick: () => void; children: ReactNode }) {
  return (
    <button title={title} aria-label={title} onClick={onClick} className="grid size-5 place-items-center rounded text-ink-3 hover:bg-active hover:text-ink">
      {children}
    </button>
  );
}

function SmallButton({ onClick, children }: { onClick: () => void; children: ReactNode }) {
  return (
    <button onClick={onClick} className="rounded px-1.5 py-0.5 text-xs text-ink-3 hover:bg-hover hover:text-ink">
      {children}
    </button>
  );
}
