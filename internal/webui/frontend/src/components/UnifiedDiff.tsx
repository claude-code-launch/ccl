import { memo } from "react";

// UnifiedDiff renders git's diff output: file headers, hunks, and the lines a
// change added or removed. Anything before the first file (a commit's header
// and stat from `git show`) is shown as is.
export const UnifiedDiff = memo(function UnifiedDiff({ diff }: { diff: string }) {
  const lines = diff.replace(/\n$/, "").split("\n");
  const rows: { kind: string; text: string }[] = [];
  let inFile = false;
  for (const line of lines) {
    if (line.startsWith("diff --git ")) {
      inFile = true;
      const match = / b\/(.*)$/.exec(line);
      rows.push({ kind: "file", text: match ? match[1] : line });
    } else if (!inFile) {
      rows.push({ kind: "meta", text: line });
    } else if (/^(index |--- |\+\+\+ |new file mode|deleted file mode|similarity index|rename (from|to) |old mode|new mode|Binary files)/.test(line)) {
      if (line.startsWith("Binary files") || line.startsWith("rename ") || line.includes("file mode")) rows.push({ kind: "note", text: line });
    } else if (line.startsWith("@@")) {
      rows.push({ kind: "hunk", text: line });
    } else if (line.startsWith("+")) {
      rows.push({ kind: "add", text: line.slice(1) });
    } else if (line.startsWith("-")) {
      rows.push({ kind: "del", text: line.slice(1) });
    } else if (line.startsWith("\\")) {
      rows.push({ kind: "note", text: line });
    } else {
      rows.push({ kind: "same", text: line.slice(1) });
    }
  }
  if (!diff.trim()) return null;
  return (
    <div className="font-mono text-[12px] leading-[1.5]">
      {rows.map((row, index) => {
        switch (row.kind) {
          case "file":
            return (
              <div key={index} className="sticky top-0 z-[1] mt-2 truncate border-y border-line bg-surface px-2.5 py-1 text-ink-2 first:mt-0">
                {row.text}
              </div>
            );
          case "meta":
            return <div key={index} className="whitespace-pre-wrap break-words px-2.5 text-ink-2">{row.text || " "}</div>;
          case "hunk":
            return <div key={index} className="bg-code px-2.5 text-ink-3">{row.text}</div>;
          case "note":
            return <div key={index} className="px-2.5 italic text-ink-3">{row.text}</div>;
          default:
            return (
              <div
                key={index}
                className={
                  "flex whitespace-pre-wrap break-all px-2.5 " +
                  (row.kind === "add"
                    ? "bg-emerald-500/10 text-emerald-800 dark:text-emerald-200"
                    : row.kind === "del"
                      ? "bg-red-500/10 text-red-800 dark:text-red-200"
                      : "text-ink-2")
                }
              >
                <span className="w-4 shrink-0 select-none text-ink-3">{row.kind === "add" ? "+" : row.kind === "del" ? "−" : " "}</span>
                <span>{row.text || " "}</span>
              </div>
            );
        }
      })}
    </div>
  );
});
