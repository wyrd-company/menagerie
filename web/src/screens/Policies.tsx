import { useEffect, useRef, useState } from "react";
import { ApiError, listPolicies, savePolicySettings } from "../api/client.ts";
import type { Policies, PolicySettings } from "../api/types.ts";

export function PoliciesScreen() {
  const [data, setData] = useState<Policies | null>(null);
  const [draft, setDraft] = useState<PolicySettings | null>(null);
  const [error, setError] = useState<ApiError | Error | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const errorRef = useRef<HTMLDivElement>(null);
  async function load() {
    setError(null);
    try {
      const result = await listPolicies();
      setData(result);
      setDraft(result.settings);
      setSaved(false);
    } catch (e) {
      setError(e instanceof Error ? e : new Error(String(e)));
    }
  }
  useEffect(() => {
    let active = true;
    void listPolicies().then(
      (result) => {
        if (active) {
          setData(result);
          setDraft(result.settings);
        }
      },
      (e) => {
        if (active) setError(e instanceof Error ? e : new Error(String(e)));
      },
    );
    return () => {
      active = false;
    };
  }, []);
  const update = (next: PolicySettings) => {
    setDraft(next);
    setSaved(false);
  };
  async function save() {
    if (!draft) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      const result = await savePolicySettings(draft);
      setDraft(result);
      setData((current) => (current ? { ...current, settings: result } : current));
      setSaved(true);
    } catch (e) {
      setError(e instanceof Error ? e : new Error(String(e)));
      requestAnimationFrame(() => errorRef.current?.focus());
    } finally {
      setSaving(false);
    }
  }
  const toggle = (names: string[], name: string) =>
    names.includes(name) ? names.filter((p) => p !== name) : [...names, name];
  return (
    <section className="screen policy-screen" aria-labelledby="policies-title">
      <header className="screen-head">
        <h1 id="policies-title">Policies</h1>
      </header>
      {!data && !error ? <p className="muted">Reading policies…</p> : null}
      {error ? (
        <div className="notice notice-bad" role="alert" tabIndex={-1} ref={errorRef}>
          <p>{error.message}</p>
          <button type="button" onClick={() => void load()}>
            {error instanceof ApiError && error.kind === "settings-changed"
              ? "Reload settings"
              : "Retry"}
          </button>
        </div>
      ) : null}
      {data?.grant === null ? (
        <p className="notice">Configure provisioning.wyrwood to manage policies.</p>
      ) : null}
      {data && draft ? (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <fieldset className="policy-panel" disabled={saving || data.grant === null}>
            <legend className="sr-only">Policy settings</legend>
            <h2>Required policies</h2>
            <div>
              {data.policies.length === 0 ? (
                <p className="muted">No policies.</p>
              ) : (
                data.policies.map((policy) => (
                  <div className="policy-settings-row" key={policy.name}>
                    <span className="mono">{policy.name}</span>
                    <label className="choice-line">
                      <input
                        type="checkbox"
                        checked={draft.required.includes(policy.name)}
                        disabled={!policy.inGrant && !draft.required.includes(policy.name)}
                        onChange={() =>
                          update({ ...draft, required: toggle(draft.required, policy.name) })
                        }
                        aria-label={`${policy.name} required`}
                      />
                      Required
                    </label>
                    <span className="policy-scope">
                      {policy.inGrant ? "Inside grant" : "Outside grant"}
                    </span>
                  </div>
                ))
              )}
            </div>
            <h2>Policy groups</h2>
            {draft.groups.map((group, i) => (
              <section
                className="group-editor"
                key={group.id ?? `new-${i}`}
                aria-label={`Group ${i + 1}`}
              >
                <div className="group-head">
                  <label>
                    Group name
                    <input
                      value={group.name}
                      required
                      maxLength={100}
                      onChange={(e) =>
                        update({
                          ...draft,
                          groups: draft.groups.map((g, n) =>
                            n === i ? { ...g, name: e.target.value } : g,
                          ),
                        })
                      }
                    />
                  </label>
                  <label>
                    Selection
                    <select
                      value={group.mode}
                      onChange={(e) =>
                        update({
                          ...draft,
                          groups: draft.groups.map((g, n) =>
                            n === i ? { ...g, mode: e.target.value as "single" | "multi" } : g,
                          ),
                        })
                      }
                    >
                      <option value="single">Single-select</option>
                      <option value="multi">Multi-select</option>
                    </select>
                  </label>
                  <button
                    type="button"
                    onClick={() =>
                      update({ ...draft, groups: draft.groups.filter((_, n) => n !== i) })
                    }
                  >
                    Remove group
                  </button>
                </div>
                <div className="group-options">
                  {data.policies
                    .filter(
                      (policy) =>
                        !draft.required.includes(policy.name) ||
                        group.policies.includes(policy.name),
                    )
                    .map((policy) => (
                      <label className="choice-line" key={policy.name}>
                        <input
                          type="checkbox"
                          checked={group.policies.includes(policy.name)}
                          disabled={!policy.inGrant && !group.policies.includes(policy.name)}
                          onChange={() =>
                            update({
                              ...draft,
                              groups: draft.groups.map((g, n) =>
                                n === i ? { ...g, policies: toggle(g.policies, policy.name) } : g,
                              ),
                            })
                          }
                        />
                        <span className="mono">{policy.name}</span>
                        {!policy.inGrant ? <small>Outside grant</small> : null}
                      </label>
                    ))}
                </div>
              </section>
            ))}
            <div>
              <button
                type="button"
                className="ghost"
                onClick={() =>
                  update({
                    ...draft,
                    groups: [...draft.groups, { name: "", mode: "single", policies: [] }],
                  })
                }
              >
                Add group
              </button>
            </div>
            <div className="policy-actions">
              <button className="primary" type="submit">
                {saving ? "Saving…" : "Save"}
              </button>
              {saved ? <span role="status">Saved</span> : null}
            </div>
          </fieldset>
        </form>
      ) : null}
    </section>
  );
}
