import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { useJob } from "./useJob.ts";

/** The browser's EventSource after a refused request: closed, and it tells onerror once. */
class RefusedEventSource extends EventTarget {
  static readonly CLOSED = 2;
  static last: RefusedEventSource | null = null;
  readyState = RefusedEventSource.CLOSED;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  close = () => {};
  constructor() {
    super();
    RefusedEventSource.last = this;
  }
}

function stubBrowser(status: number) {
  const fetchStub = vi.fn(async () => new Response(null, { status }));
  vi.stubGlobal("EventSource", RefusedEventSource);
  vi.stubGlobal("fetch", fetchStub);
  vi.stubGlobal("location", { pathname: "/", search: "", assign: () => {} });
  return fetchStub;
}

async function refuseStream(status: number) {
  const fetchStub = stubBrowser(status);
  const { result } = renderHook(() => useJob("j1"));
  await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));
  await act(async () => {
    RefusedEventSource.last!.onerror!();
    await Promise.resolve();
  });
  await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(2));
  // Let the second read's refusal reach the hook.
  await act(async () => {
    await new Promise((settled) => setTimeout(settled));
  });
  return result;
}

describe("a refused event stream", () => {
  it("is not reported as lost when the session is gone", async () => {
    const result = await refuseStream(401);
    expect(result.current.view.connection).not.toBe("lost");
  });

  it("is reported as lost when the read fails another way", async () => {
    const result = await refuseStream(500);
    expect(result.current.view.connection).toBe("lost");
  });
});
