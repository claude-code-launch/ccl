// A line diff for tool edits (old_string → new_string). Edits are small, so
// an LCS table is fine; past the size cap it degrades to "all removed, all
// added", which is still a correct diff.

export type DiffLine = { type: "same" | "add" | "del"; text: string };

const CELL_LIMIT = 2_000_000;

export function diffLines(before: string, after: string): DiffLine[] {
  const a = splitLines(before);
  const b = splitLines(after);
  // Common head and tail need no table.
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA--;
    endB--;
  }
  const head: DiffLine[] = a.slice(0, start).map((text) => ({ type: "same", text }));
  const tail: DiffLine[] = a.slice(endA).map((text) => ({ type: "same", text }));
  const midA = a.slice(start, endA);
  const midB = b.slice(start, endB);
  return [...head, ...diffMiddle(midA, midB), ...tail];
}

function diffMiddle(a: string[], b: string[]): DiffLine[] {
  if (!a.length) return b.map((text) => ({ type: "add", text }));
  if (!b.length) return a.map((text) => ({ type: "del", text }));
  if (a.length * b.length > CELL_LIMIT) {
    return [...a.map((text) => ({ type: "del" as const, text })), ...b.map((text) => ({ type: "add" as const, text }))];
  }
  // lengths[i][j] = LCS of a[i:] and b[j:]
  const width = b.length + 1;
  const lengths = new Uint32Array((a.length + 1) * width);
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lengths[i * width + j] =
        a[i] === b[j] ? lengths[(i + 1) * width + j + 1] + 1 : Math.max(lengths[(i + 1) * width + j], lengths[i * width + j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      out.push({ type: "same", text: a[i] });
      i++;
      j++;
    } else if (lengths[(i + 1) * width + j] >= lengths[i * width + j + 1]) {
      out.push({ type: "del", text: a[i++] });
    } else {
      out.push({ type: "add", text: b[j++] });
    }
  }
  while (i < a.length) out.push({ type: "del", text: a[i++] });
  while (j < b.length) out.push({ type: "add", text: b[j++] });
  return out;
}

function splitLines(text: string): string[] {
  if (!text) return [];
  const lines = text.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}

export function diffStats(lines: DiffLine[]): { added: number; deleted: number } {
  let added = 0;
  let deleted = 0;
  for (const line of lines) {
    if (line.type === "add") added++;
    else if (line.type === "del") deleted++;
  }
  return { added, deleted };
}

// Collapse unchanged runs longer than 2×context into a marker.
export function withContext(lines: DiffLine[], context = 3): (DiffLine | { type: "gap"; count: number })[] {
  const out: (DiffLine | { type: "gap"; count: number })[] = [];
  let i = 0;
  while (i < lines.length) {
    if (lines[i].type !== "same") {
      out.push(lines[i++]);
      continue;
    }
    let j = i;
    while (j < lines.length && lines[j].type === "same") j++;
    const run = j - i;
    const atStart = i === 0;
    const atEnd = j === lines.length;
    const keepBefore = atStart ? 0 : context;
    const keepAfter = atEnd ? 0 : context;
    if (run > keepBefore + keepAfter + 1) {
      out.push(...lines.slice(i, i + keepBefore));
      out.push({ type: "gap", count: run - keepBefore - keepAfter });
      out.push(...lines.slice(j - keepAfter, j));
    } else {
      out.push(...lines.slice(i, j));
    }
    i = j;
  }
  return out;
}
