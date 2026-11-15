import { useState } from "react";
import { CircleCheck, CircleSlash, CircleX, MessageCircleQuestion, ShieldQuestion } from "lucide-react";
import type { PermissionItem } from "../thread";
import type { PermissionDecision } from "../types";
import { useI18n } from "../i18n";
import { Markdown } from "./Markdown";

interface Props {
  item: PermissionItem;
  onRespond: (decision: PermissionDecision) => Promise<void>;
}

interface Question {
  question: string;
  header?: string;
  multiSelect?: boolean;
  options: { label: string; description?: string }[];
}

// PermissionCard is a question Claude Code asked the user: whether a tool may
// run, an AskUserQuestion, or a plan to approve. Once answered it shrinks to
// one line.
export function PermissionCard({ item, onRespond }: Props) {
  const { t } = useI18n();
  const [busy, setBusy] = useState(false);

  if (item.status !== "pending") {
    const label =
      item.status === "allow" ? t("permAllowed", { tool: item.toolName }) : item.status === "deny" ? t("permDenied", { tool: item.toolName }) : t("permWithdrawn", { tool: item.toolName });
    return (
      <div className="flex items-center gap-1.5 py-0.5 pl-1 text-[13px] text-ink-3">
        {item.status === "allow" ? <CircleCheck size={13} className="text-emerald-500" /> : item.status === "deny" ? <CircleX size={13} className="text-red-500" /> : <CircleSlash size={13} />}
        {label}
        {item.message && <span className="truncate">· {item.message}</span>}
      </div>
    );
  }

  const respond = async (decision: Omit<PermissionDecision, "requestId">) => {
    setBusy(true);
    try {
      await onRespond({ requestId: item.requestId, ...decision });
    } finally {
      setBusy(false);
    }
  };

  const shell = "rounded-[12px] border border-clay/50 bg-clay/[0.06] px-3.5 py-3 my-1";
  if (item.toolName === "AskUserQuestion" && Array.isArray(item.input?.questions)) {
    return <Questions item={item} questions={item.input.questions as Question[]} busy={busy} respond={respond} className={shell} />;
  }
  if (item.toolName === "ExitPlanMode") {
    return <PlanApproval item={item} busy={busy} respond={respond} className={shell} />;
  }

  const detail = String(item.input?.command || item.input?.file_path || item.summary || "");
  return (
    <div className={shell}>
      <div className="mb-2 flex items-center gap-2 text-[14px] text-ink">
        <ShieldQuestion size={16} className="shrink-0 text-clay" />
        <span className="font-medium">{item.title || t("permAsk", { tool: item.toolName })}</span>
      </div>
      {item.description && <p className="mb-2 text-[13.5px] text-ink-2">{item.description}</p>}
      {detail && (
        <pre className="mb-2 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-line bg-code px-2.5 py-2 font-mono text-[12.5px] text-ink">
          {item.toolName === "Bash" && <span className="select-none text-ink-3">$ </span>}
          {detail}
        </pre>
      )}
      {item.reason && <p className="mb-2 text-[12.5px] text-ink-3">{item.reason}</p>}
      <DenyRow
        busy={busy}
        onDeny={(message) => respond({ behavior: "deny", message })}
        primary={
          <>
            <button disabled={busy} onClick={() => respond({ behavior: "allow" })} className="rounded-lg bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-50">
              {t("permAllow")}
            </button>
            {item.canRemember && (
              <button disabled={busy} onClick={() => respond({ behavior: "allow", remember: true })} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-ink hover:bg-hover disabled:opacity-50">
                {t("permAllowSession")}
              </button>
            )}
          </>
        }
      />
    </div>
  );
}

// DenyRow puts the primary answers first and lets a refusal carry a reason.
function DenyRow({ busy, primary, onDeny, denyLabel }: { busy: boolean; primary: React.ReactNode; onDeny: (message: string) => void; denyLabel?: string }) {
  const { t } = useI18n();
  const [reason, setReason] = useState("");
  return (
    <div className="flex flex-wrap items-center gap-2">
      {primary}
      <input
        value={reason}
        onChange={(event) => setReason(event.target.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && !event.nativeEvent.isComposing) onDeny(reason);
        }}
        placeholder={t("permDenyReason")}
        className="min-w-[160px] flex-1 rounded-lg border border-line bg-surface px-2.5 py-1.5 text-[13px] text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
      />
      <button disabled={busy} onClick={() => onDeny(reason)} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-ink-2 hover:bg-hover disabled:opacity-50">
        {denyLabel || t("permDeny")}
      </button>
    </div>
  );
}

function Questions({ item, questions, busy, respond, className }: {
  item: PermissionItem;
  questions: Question[];
  busy: boolean;
  respond: (decision: Omit<PermissionDecision, "requestId">) => Promise<void>;
  className: string;
}) {
  const { t } = useI18n();
  const [picked, setPicked] = useState<Record<string, string[]>>({});
  const [other, setOther] = useState<Record<string, string>>({});
  const answer = (question: Question) => {
    const chosen = [...(picked[question.question] || [])];
    if (other[question.question]?.trim()) chosen.push(other[question.question].trim());
    return chosen.join(", ");
  };
  const complete = questions.every((question) => answer(question));
  return (
    <div className={className}>
      <div className="mb-2 flex items-center gap-2 text-[14px] font-medium text-ink">
        <MessageCircleQuestion size={16} className="shrink-0 text-clay" />
        {item.title || t("permQuestion")}
      </div>
      <div className="space-y-3">
        {questions.map((question) => (
          <div key={question.question}>
            <p className="mb-1.5 text-[14px] text-ink">
              {question.header && <span className="mr-1.5 rounded bg-hover px-1.5 py-0.5 text-[11px] text-ink-2">{question.header}</span>}
              {question.question}
            </p>
            <div className="space-y-1">
              {(question.options || []).map((option) => {
                const selected = (picked[question.question] || []).includes(option.label);
                return (
                  <button
                    key={option.label}
                    onClick={() =>
                      setPicked((all) => {
                        const current = all[question.question] || [];
                        const next = question.multiSelect
                          ? selected ? current.filter((label) => label !== option.label) : [...current, option.label]
                          : selected ? [] : [option.label];
                        return { ...all, [question.question]: next };
                      })
                    }
                    className={"flex w-full items-start gap-2 rounded-lg border px-2.5 py-1.5 text-left text-[13.5px] " + (selected ? "border-clay bg-clay/10 text-ink" : "border-line text-ink-2 hover:bg-hover")}
                  >
                    <span className={"mt-1 size-3 shrink-0 border " + (question.multiSelect ? "rounded-sm" : "rounded-full") + (selected ? " border-clay bg-clay" : " border-line-strong")} />
                    <span>
                      {option.label}
                      {option.description && <span className="block text-[12px] text-ink-3">{option.description}</span>}
                    </span>
                  </button>
                );
              })}
              <input
                value={other[question.question] || ""}
                onChange={(event) => setOther((all) => ({ ...all, [question.question]: event.target.value }))}
                placeholder={t("permOther")}
                className="w-full rounded-lg border border-line bg-surface px-2.5 py-1.5 text-[13px] text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
              />
            </div>
          </div>
        ))}
      </div>
      <div className="mt-3">
        <DenyRow
          busy={busy}
          denyLabel={t("permSkip")}
          onDeny={(message) => respond({ behavior: "deny", message: message || "The user skipped the question." })}
          primary={
            <button
              disabled={busy || !complete}
              onClick={() => respond({ behavior: "allow", answers: Object.fromEntries(questions.map((question) => [question.question, answer(question)])) })}
              className="rounded-lg bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-50"
            >
              {t("permSubmit")}
            </button>
          }
        />
      </div>
    </div>
  );
}

function PlanApproval({ item, busy, respond, className }: {
  item: PermissionItem;
  busy: boolean;
  respond: (decision: Omit<PermissionDecision, "requestId">) => Promise<void>;
  className: string;
}) {
  const { t } = useI18n();
  const plan = String(item.input?.plan || "");
  return (
    <div className={className}>
      <div className="mb-2 text-[14px] font-medium text-ink">{t("planReady")}</div>
      {plan && (
        <div className="mb-3 max-h-[420px] overflow-auto rounded-lg border border-line bg-surface px-3 py-2">
          <Markdown text={plan} />
        </div>
      )}
      <DenyRow
        busy={busy}
        denyLabel={t("planKeep")}
        onDeny={(message) => respond({ behavior: "deny", message: message ? `Keep planning: ${message}` : "Keep planning; the plan is not approved yet." })}
        primary={
          <>
            <button disabled={busy} onClick={() => respond({ behavior: "allow", mode: "acceptEdits" })} className="rounded-lg bg-clay px-3 py-1.5 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-50">
              {t("planApproveAuto")}
            </button>
            <button disabled={busy} onClick={() => respond({ behavior: "allow", mode: "default" })} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-ink hover:bg-hover disabled:opacity-50">
              {t("planApproveAsk")}
            </button>
          </>
        }
      />
    </div>
  );
}
