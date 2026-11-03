import { beforeEach, describe, expect, it, vi } from "vitest";

const assign = vi.fn<(url: string) => void>();

/** A fresh module is a fresh page: nothing has been redirected yet. */
async function loadPage() {
  vi.resetModules();
  assign.mockClear();
  vi.stubGlobal("location", { pathname: "/sample", search: "?job=j1", assign });
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(null, { status: 401 })),
  );
  return import("./client.ts");
}

const restore = (persisted: boolean) =>
  window.dispatchEvent(new PageTransitionEvent("pageshow", { persisted }));

describe("a lost session", () => {
  beforeEach(() => vi.unstubAllGlobals());

  it("starts one redirect for two 401s on one page", async () => {
    const { ApiError, listJobs, listWorkspaces } = await loadPage();
    const reads = await Promise.allSettled([listJobs(), listWorkspaces()]);
    expect(assign.mock.calls).toEqual([["/auth/login?return=%2Fsample%3Fjob%3Dj1"]]);
    // Every read still fails, so that no caller takes it for an answer.
    for (const read of reads) {
      expect(read.status).toBe("rejected");
      const { reason } = read as PromiseRejectedResult;
      expect(reason).toBeInstanceOf(ApiError);
      expect((reason as InstanceType<typeof ApiError>).status).toBe(401);
    }
  });

  it("starts a new redirect on a page the back/forward cache restored", async () => {
    const { listJobs } = await loadPage();
    await expect(listJobs()).rejects.toThrow();
    restore(true);
    await expect(listJobs()).rejects.toThrow();
    expect(assign).toHaveBeenCalledTimes(2);
  });

  it.each(["pointerdown", "keydown"])("starts a new redirect after a person's %s", async (type) => {
    const { listJobs } = await loadPage();
    await expect(listJobs()).rejects.toThrow();
    window.dispatchEvent(new Event(type));
    await expect(listJobs()).rejects.toThrow();
    expect(assign).toHaveBeenCalledTimes(2);
  });

  it("starts no new redirect on a page shown without a restore", async () => {
    const { listJobs } = await loadPage();
    await expect(listJobs()).rejects.toThrow();
    restore(false);
    await expect(listJobs()).rejects.toThrow();
    expect(assign).toHaveBeenCalledTimes(1);
  });
});
