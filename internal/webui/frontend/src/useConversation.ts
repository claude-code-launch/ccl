import { useEffect, useRef, useState } from "react";
import type { Api } from "./api";
import type { Session, StoredEvent } from "./types";

const STREAM_TYPES = ["init", "token", "thinking", "context", "tool_use", "tool_result", "permission", "permission_resolved", "result", "error", "queued", "status", "user"];

export interface Conversation {
  events: StoredEvent[];
  running: boolean;
  loading: boolean;
  error: string;
  setRunning: (running: boolean) => void;
}

// useConversation replays a conversation's history and follows its stream.
// onBoundary runs when a turn starts or ends, so the caller can refresh the
// conversation list; onSession receives the server's view of the session.
export function useConversation(
  api: Api,
  id: string | null,
  onBoundary: () => void,
  onSession: (session: Session) => void,
): Conversation {
  const [events, setEvents] = useState<StoredEvent[]>([]);
  const [running, setRunning] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const callbacks = useRef({ onBoundary, onSession });
  callbacks.current = { onBoundary, onSession };

  useEffect(() => {
    setEvents([]);
    setError("");
    setRunning(false);
    if (!id) return;

    let closed = false;
    let source: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let frame = 0;
    let lastSeq = 0;
    const buffer: StoredEvent[] = [];

    // A streamed answer appends an event per token; render once per frame.
    const flush = () => {
      frame = 0;
      if (!closed) setEvents(buffer.slice());
    };
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(flush);
    };

    const open = () => {
      if (closed) return;
      // EventSource reconnects by itself but without ?since=; a fresh one with
      // the last seq seen is what keeps the history gap-free.
      const stream = new EventSource(api.streamURL(id, lastSeq));
      source = stream;
      for (const type of STREAM_TYPES) {
        stream.addEventListener(type, (message) => {
          const data = (message as MessageEvent<string>).data;
          let payload: unknown = {};
          try {
            payload = JSON.parse(data);
          } catch {
            payload = {};
          }
          const seq = Number((message as MessageEvent).lastEventId) || lastSeq + 1;
          if (seq <= lastSeq) return;
          lastSeq = seq;
          buffer.push({ seq, type, payload, ts: Date.now() });
          schedule();
          if (type === "user") setRunning(true); // possibly from another tab
          if (type === "result") setRunning(false);
          if (type === "user" || type === "status" || type === "result") callbacks.current.onBoundary();
        });
      }
      stream.onerror = () => {
        if (source !== stream) return;
        stream.close();
        source = null;
        if (!closed) retry = setTimeout(open, 3000);
      };
    };

    setLoading(true);
    (async () => {
      try {
        const history = await api.events(id, 0);
        if (closed) return;
        for (const event of history) {
          buffer.push(event);
          lastSeq = Math.max(lastSeq, event.seq);
        }
        flush();
        // Asked after the history: a turn that ends in between still delivers
        // its result through the stream, and one that starts, its user event.
        const fresh = await api.session(id);
        if (closed) return;
        setRunning(!!fresh.running);
        callbacks.current.onSession(fresh);
        open();
      } catch (err) {
        if (!closed) setError(err instanceof Error ? err.message : String(err));
      } finally {
        if (!closed) setLoading(false);
      }
    })();

    return () => {
      closed = true;
      source?.close();
      if (retry) clearTimeout(retry);
      if (frame) cancelAnimationFrame(frame);
    };
  }, [api, id]);

  return { events, running, loading, error, setRunning };
}
