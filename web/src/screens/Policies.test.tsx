import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, expect, it, vi } from "vitest";
import { PoliciesScreen } from "./Policies.tsx";
import type { Policies, PolicySettings } from "../api/types.ts";

const example = (): Policies => ({
  grant: { agents: ["host"], policies: ["*"], actions: ["read", "bind"] },
  policies: [
    { name: "workspace", valueNames: [], inGrant: true },
    { name: "one", valueNames: [], inGrant: true },
    { name: "two", valueNames: [], inGrant: true },
    { name: "lost", valueNames: [], inGrant: false },
  ],
  settings: { required: [], groups: [], revision: 3 },
});
afterEach(() => vi.unstubAllGlobals());
function stub(data: Policies, refusal?: string) {
  const writes: PolicySettings[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_: string, init?: RequestInit) => {
      if (init?.method === "PUT") {
        const body = JSON.parse(String(init.body)) as PolicySettings;
        writes.push(body);
        if (refusal)
          return Response.json(
            {
              error: {
                kind: refusal,
                message: "Policy settings changed. Reload them and try again.",
              },
            },
            { status: 409 },
          );
        return Response.json({
          ...body,
          revision: body.revision + 1,
          groups: body.groups.map((g, i) => ({ ...g, id: g.id ?? i + 1 })),
        });
      }
      return Response.json(data);
    }),
  );
  return writes;
}
it("edits required policies and groups, then saves without an extra step", async () => {
  const writes = stub(example());
  render(<PoliciesScreen />);
  await userEvent.click(await screen.findByRole("checkbox", { name: "workspace required" }));
  await userEvent.click(screen.getByRole("button", { name: "Add group" }));
  const group = screen.getByRole("region", { name: "Group 1" });
  await userEvent.type(within(group).getByRole("textbox", { name: "Group name" }), "Operator SSH");
  await userEvent.selectOptions(
    within(group).getByRole("combobox", { name: "Selection" }),
    "multi",
  );
  await userEvent.click(within(group).getByRole("checkbox", { name: "one" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  await screen.findByRole("status");
  expect(writes[0]).toEqual({
    required: ["workspace"],
    groups: [{ name: "Operator SSH", mode: "multi", policies: ["one"] }],
    revision: 3,
  });
  await userEvent.click(screen.getByRole("button", { name: "Remove group" }));
  await userEvent.click(screen.getByRole("checkbox", { name: "workspace required" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(writes).toHaveLength(2));
  expect(writes[1]).toEqual({ required: [], groups: [], revision: 4 });
});
it("shows outside-grant policies and lets a manager remove existing references", async () => {
  const data = example();
  data.settings.required = ["lost"];
  stub(data);
  render(<PoliciesScreen />);
  const required = await screen.findByRole("checkbox", { name: "lost required" });
  expect(required).toBeEnabled();
  await userEvent.click(required);
  expect(required).toBeDisabled();
  expect(screen.getByText("Outside grant")).toBeVisible();
});
it("retains the draft on a stale save and offers a fresh read", async () => {
  const writes = stub(example(), "settings-changed");
  render(<PoliciesScreen />);
  await userEvent.click(await screen.findByRole("checkbox", { name: "workspace required" }));
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Policy settings changed");
  expect(screen.getByRole("checkbox", { name: "workspace required" })).toBeChecked();
  expect(writes).toHaveLength(1);
  await userEvent.click(screen.getByRole("button", { name: "Reload settings" }));
  await waitFor(() =>
    expect(screen.getByRole("checkbox", { name: "workspace required" })).not.toBeChecked(),
  );
});
it("disables settings when provisioning is absent and offers retry after an outage", async () => {
  const data = example();
  data.grant = null;
  stub(data);
  const view = render(<PoliciesScreen />);
  expect(await screen.findByRole("button", { name: "Save" })).toBeDisabled();
  view.unmount();
  vi.stubGlobal(
    "fetch",
    vi.fn(async () =>
      Response.json(
        { error: { kind: "wyrwood-unavailable", message: "Controller unavailable" } },
        { status: 503 },
      ),
    ),
  );
  render(<PoliciesScreen />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Controller unavailable");
  expect(screen.getByRole("button", { name: "Retry" })).toBeEnabled();
});
