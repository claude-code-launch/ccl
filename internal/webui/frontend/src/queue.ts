import type { StoredEvent, UserPayload } from "./types";

// Queued is a message written while a turn runs. It goes out when the turn
// ends, or into the running turn once steered.
export interface Queued {
  key: string;
  text: string;
  attachments: string[];
  // Set once the message went into the running turn: the newest event seq
  // then. Steered messages lead the queue, in the order they were sent.
  steered?: number;
}

// settleQueue drops the steered messages Claude Code has read (each one comes
// back as a steered user event, in order). When the turn is over, one it never
// read (the turn was stopped) becomes an ordinary queued message again;
// sending keeps a message whose request is still out. It returns null when
// nothing changes.
export function settleQueue(queue: Queued[], events: StoredEvent[], running: boolean, sending: ReadonlySet<string>): Queued[] | null {
  const first = queue[0];
  if (first?.steered === undefined) return null;
  let read = events.filter((event) => event.type === "user" && event.seq > first.steered! && (event.payload as UserPayload).steered).length;
  let changed = false;
  const next: Queued[] = [];
  for (const item of queue) {
    if (item.steered !== undefined && read > 0) {
      read--;
      changed = true;
      continue;
    }
    if (item.steered !== undefined && !running && !sending.has(item.key)) {
      next.push({ ...item, steered: undefined });
      changed = true;
      continue;
    }
    next.push(item);
  }
  return changed ? next : null;
}

// steerFirst moves a message to the end of the steered ones at the head of
// the queue and marks it sent after event seq since.
export function steerFirst(queue: Queued[], key: string, since: number): Queued[] {
  const item = queue.find((entry) => entry.key === key);
  if (!item) return queue;
  const rest = queue.filter((entry) => entry !== item);
  const at = rest.findIndex((entry) => entry.steered === undefined);
  const index = at < 0 ? rest.length : at;
  return [...rest.slice(0, index), { ...item, steered: since }, ...rest.slice(index)];
}
