import { useId } from "react";
import type { Policies } from "../api/types.ts";

export function PolicyChoices({
  catalog,
  selected,
  onChange,
  disabled = false,
}: {
  catalog: Policies;
  selected: string[];
  onChange: (policies: string[]) => void;
  disabled?: boolean;
}) {
  const id = useId();
  const inGrant = (name: string) => catalog.policies.some((p) => p.name === name && p.inGrant);
  const line = (name: string, required = false, group?: Policies["settings"]["groups"][number]) => (
    <label className="choice-line" key={name}>
      <input
        type={group?.mode === "single" ? "radio" : "checkbox"}
        name={`${id}-${group?.id ?? group?.name ?? name}`}
        value={name}
        checked={selected.includes(name)}
        disabled={disabled || required || (!inGrant(name) && !selected.includes(name))}
        onChange={(event) => {
          const rest = selected.filter(
            (p) => p !== name && !(group?.mode === "single" && group.policies.includes(p)),
          );
          onChange(event.target.checked ? [...rest, name] : rest);
        }}
      />
      <span className="mono">{name}</span>
      {required ? <small>Required</small> : null}
      {!inGrant(name) ? <small className="field-errors">Outside grant</small> : null}
    </label>
  );
  const used = [
    ...catalog.settings.required,
    ...catalog.settings.groups.flatMap((g) => g.policies),
  ];
  const ordinary = [...new Set([...catalog.policies.map((p) => p.name), ...selected])]
    .filter((p) => !used.includes(p) && (inGrant(p) || selected.includes(p)))
    .sort();
  return (
    <div className="policy-columns">
      {catalog.settings.required.length ? (
        <fieldset>
          <legend>Required</legend>
          <div className="choices-list">{catalog.settings.required.map((p) => line(p, true))}</div>
        </fieldset>
      ) : null}
      {catalog.settings.groups.map((g) => (
        <fieldset key={g.id ?? g.name}>
          <legend>
            {g.name}{" "}
            <span className="muted">
              {g.mode === "single" ? "Choose one" : "Choose at least one"}
            </span>
          </legend>
          <div className="choices-list">{g.policies.map((p) => line(p, false, g))}</div>
        </fieldset>
      ))}
      {ordinary.length ? (
        <fieldset>
          <legend>Other policies</legend>
          <div className="choices-list">{ordinary.map((p) => line(p))}</div>
        </fieldset>
      ) : null}
      {!used.length && !ordinary.length ? (
        <p className="muted">No policies in the operator key’s grant.</p>
      ) : null}
    </div>
  );
}

export function choiceProblems(
  catalog: Policies,
  selected: string[],
  previous?: string[],
): string[] {
  return catalog.settings.groups.flatMap((g) => {
    const valid = (names: string[]) => {
      const count = g.policies.filter((p) => names.includes(p)).length;
      return count > 0 && (g.mode !== "single" || count === 1);
    };
    return !valid(selected) && (previous === undefined || valid(previous))
      ? [
          `Choose ${g.mode === "single" ? "exactly one policy" : "at least one policy"} in ${g.name}.`,
        ]
      : [];
  });
}
