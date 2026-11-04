import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Api } from "./api";
import type { StoredEvent } from "./types";
import { useConversation } from "./useConversation";

// Run the hook's effect with controllable browser events and timers, without
// mounting a UI or connecting to a real conversation.
const hooks = vi.hoisted(() => ({
  effects: [] as (() => void | (() => void))[],
  values: [] as unknown[],
}));
vi.mock("react", () => ({
  useEffect: (effect: () => void | (() => void)) => hooks.effects.push(effect),
  useState: (initial: unknown) => {
    const index = hooks.values.length;
    hooks.values.push(initial);
    return [initial, (value: unknown) => { hooks.values[index] = value; }];
  },
  useRef: (value: unknown) => ({ current: value }),
}));

class Stream extends EventTarget {
  static instances: Stream[] = [];
  onerror: ((event: Event) => void) | null = null;
  close = vi.fn();

  constructor(public url: string) {
    super();
    Stream.instances.push(this);
  }

  dispatchEvent(event: Event): boolean {
    const result = super.dispatchEvent(event);
    if (event.type === "error") this.onerror?.(event);
    return result;
  }
}

describe("conversation stream errors", () => {
  let cleanup: (() => void) | undefined;

  beforeEach(() => {
    hooks.effects = [];
    hooks.values = [];
    Stream.instances = [];
    vi.useFakeTimers();
    vi.stubGlobal("EventSource", Stream);
    vi.stubGlobal("requestAnimationFrame", (callback: FrameRequestCallback) => setTimeout(() => callback(0), 16));
    vi.stubGlobal("cancelAnimationFrame", clearTimeout);
  });

  afterEach(() => {
    cleanup?.();
    cleanup = undefined;
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  const events = () => hooks.values[0] as StoredEvent[];
  const latest = () => Stream.instances[Stream.instances.length - 1];
  const start = async () => {
    const api = {
      events: vi.fn().mockResolvedValue([{ seq: 59, type: "result", payload: { isError: false }, ts: 1 }]),
      session: vi.fn().mockResolvedValue({ id: "history", running: false }),
      streamURL: (_id: string, since: number) => `/stream?since=${since}`,
    } as unknown as Api;
    useConversation(api, "history", vi.fn(), vi.fn());
    cleanup = hooks.effects[0]() || undefined;
    await vi.advanceTimersByTimeAsync(0);
  };

  it("reconnects idle history without adding errors or skipping the next real event", async () => {
    await start();
    for (let attempt = 0; attempt < 5; attempt++) {
      const stream = latest();
      stream.dispatchEvent(new Event("error"));
      expect(stream.close).toHaveBeenCalledOnce();
      await vi.advanceTimersByTimeAsync(3000);
      expect(latest().url).toBe("/stream?since=59");
      expect(events().map((event) => event.type)).toEqual(["result"]);
    }
    latest().dispatchEvent(new MessageEvent("token", { data: JSON.stringify({ text: "continued" }), lastEventId: "60" }));
    await vi.advanceTimersByTimeAsync(16);
    expect(events().map((event) => event.seq)).toEqual([59, 60]);
    expect(events()[1].payload).toEqual({ text: "continued" });
  });

  it("displays a real server error without treating it as a disconnect", async () => {
    await start();
    const stream = latest();
    stream.dispatchEvent(new MessageEvent("error", { data: JSON.stringify({ message: "Upstream unavailable" }), lastEventId: "60" }));
    await vi.advanceTimersByTimeAsync(3000);
    expect(events()[1]).toMatchObject({ seq: 60, type: "error", payload: { message: "Upstream unavailable" } });
    expect(stream.close).not.toHaveBeenCalled();
    expect(Stream.instances).toHaveLength(1);
  });
});
