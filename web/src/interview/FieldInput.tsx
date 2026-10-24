import { useEffect, useId, useRef, type ReactNode } from "react";
import type { Choice, FieldValue } from "./answers.ts";
import type { Field } from "./fields.ts";

interface Props {
  field: Field;
  value: FieldValue;
  errors: string[];
  onChange: (value: FieldValue) => void;
}

const typeLabel: Record<Field["kind"], string> = {
  text: "text",
  git: "git remote",
  multiline: "multiline text",
  "text-list": "list",
  confirm: "yes / no",
  select: "one choice",
  multiselect: "choices",
  document: "JSON document",
  json: "JSON value",
};

function hints(field: Field): string[] {
  const out: string[] = [];
  const range = (min?: number, max?: number, unit = "characters") => {
    if (min !== undefined && max !== undefined) out.push(`${min}–${max} ${unit}`);
    else if (min !== undefined) out.push(`at least ${min} ${unit}`);
    else if (max !== undefined) out.push(`at most ${max} ${unit}`);
  };
  switch (field.kind) {
    case "text":
    case "git":
    case "multiline":
      range(field.rules.minLength, field.rules.maxLength);
      if (field.rules.pattern) out.push(`must match ${field.rules.pattern}`);
      break;
    case "text-list":
      range(field.minItems, field.maxItems, "items");
      range(field.item.minLength, field.item.maxLength, "characters each");
      if (field.item.pattern) out.push(`each must match ${field.item.pattern}`);
      break;
    case "multiselect":
      range(field.minItems, field.maxItems, "choices");
      break;
    case "document":
      if (field.schemas.length) out.push(`schema ${field.schemas.join(" or ")}`);
      if (field.input) out.push(`left unchanged, Toha reads ${field.input} from the workspace`);
      break;
    case "json":
      out.push(`x-toha-type ${field.tohaType || "(none)"}: answered as raw JSON`);
      break;
  }
  if (!field.required && field.nullable) out.push("optional");
  return out;
}

export function FieldInput({ field, value, errors, onChange }: Props) {
  const uid = useId();
  const inputId = `q-${field.id}`;
  const describedBy = [
    field.description ? `${uid}-desc` : "",
    `${uid}-hints`,
    errors.length ? `${uid}-errors` : "",
  ]
    .filter(Boolean)
    .join(" ");
  const invalid = errors.length > 0;
  const common = { "aria-describedby": describedBy, "aria-invalid": invalid || undefined };

  let control: ReactNode;
  let grouped = false;
  switch (field.kind) {
    case "text":
    case "git":
      control = (
        <input
          id={inputId}
          name={field.id}
          type={field.kind === "git" ? "url" : "text"}
          inputMode={field.kind === "git" ? "url" : undefined}
          className={field.kind === "git" ? "mono" : undefined}
          autoComplete="off"
          spellCheck={field.kind !== "git"}
          placeholder={field.placeholder}
          required={field.required}
          value={value.kind === "text" ? value.text : ""}
          onChange={(e) => onChange({ kind: "text", text: e.target.value })}
          {...common}
        />
      );
      break;
    case "multiline":
      control = (
        <textarea
          id={inputId}
          name={field.id}
          rows={4}
          placeholder={field.placeholder}
          required={field.required}
          value={value.kind === "text" ? value.text : ""}
          onChange={(e) => onChange({ kind: "text", text: e.target.value })}
          {...common}
        />
      );
      break;
    case "document":
    case "json":
      control = (
        <textarea
          id={inputId}
          name={field.id}
          className="mono"
          rows={field.kind === "document" ? 7 : 4}
          spellCheck={false}
          value={value.kind === "json" ? value.text : ""}
          onChange={(e) => onChange({ kind: "json", text: e.target.value })}
          {...common}
        />
      );
      break;
    case "text-list":
      control = <ListEditor field={field} value={value} onChange={onChange} common={common} />;
      grouped = true;
      break;
    case "confirm":
      grouped = true;
      control = (
        <ChoiceGroup
          name={field.id}
          options={[
            { label: "Yes", value: "true" },
            { label: "No", value: "false" },
            ...(field.nullable ? [{ label: "No answer", value: "null" }] : []),
          ]}
          selected={
            value.kind === "flag" && value.flag !== undefined ? String(value.flag) : undefined
          }
          onSelect={(v) => onChange({ kind: "flag", flag: v === "null" ? null : v === "true" })}
          common={common}
        />
      );
      break;
    case "select":
      grouped = true;
      control = (
        <ChoiceGroup
          name={field.id}
          options={[
            ...field.options.map((o) => ({ label: o, value: `=${o}` })),
            ...(field.nullable ? [{ label: "No answer", value: "null" }] : []),
          ]}
          selected={choiceKey(value.kind === "choice" ? value.choice : undefined)}
          onSelect={(v) => onChange({ kind: "choice", choice: v === "null" ? null : v.slice(1) })}
          common={common}
        />
      );
      break;
    case "multiselect": {
      grouped = true;
      const chosen = value.kind === "set" ? value.values : [];
      control = (
        <div className="choices" role="group" {...common}>
          {field.options.map((option) => (
            <label key={option} className="choice">
              <input
                type="checkbox"
                name={field.id}
                checked={chosen.includes(option)}
                onChange={(e) =>
                  onChange({
                    kind: "set",
                    // Keep the option order the schema gave.
                    values: field.options.filter((o) =>
                      o === option ? e.target.checked : chosen.includes(o),
                    ),
                  })
                }
              />
              <span>{option}</span>
            </label>
          ))}
        </div>
      );
      break;
    }
  }

  const Wrapper = grouped ? "fieldset" : "div";
  const labelText = (
    <>
      <span className="field-title">{field.title}</span>
      {field.required ? (
        <span className="required" aria-label="required">
          ✱
        </span>
      ) : null}
      <span className="field-type">{typeLabel[field.kind]}</span>
    </>
  );
  return (
    <Wrapper
      className={`field field-${field.kind}${invalid ? " is-invalid" : ""}`}
      data-question={field.id}
    >
      {grouped ? (
        <legend className="field-label">{labelText}</legend>
      ) : (
        <label className="field-label" htmlFor={inputId}>
          {labelText}
        </label>
      )}
      {field.description ? (
        <p className="field-description" id={`${uid}-desc`}>
          {field.description}
        </p>
      ) : null}
      {control}
      <p className="field-hints" id={`${uid}-hints`}>
        <span className="mono field-id">{field.id}</span>
        {hints(field).map((h) => (
          <span key={h}>{h}</span>
        ))}
      </p>
      {invalid ? (
        <ul className="field-errors" id={`${uid}-errors`}>
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      ) : null}
    </Wrapper>
  );
}

function choiceKey(choice: Choice): string | undefined {
  if (choice === undefined) return undefined;
  return choice === null ? "null" : `=${choice}`;
}

function ChoiceGroup(props: {
  name: string;
  options: { label: string; value: string }[];
  selected: string | undefined;
  onSelect: (value: string) => void;
  common: Record<string, unknown>;
}) {
  return (
    <div className="choices" role="radiogroup" {...props.common}>
      {props.options.map((option) => (
        <label
          key={option.value}
          className={`choice${option.value === "null" ? " choice-null" : ""}`}
        >
          <input
            type="radio"
            name={props.name}
            value={option.value}
            checked={props.selected === option.value}
            onChange={() => props.onSelect(option.value)}
          />
          <span>{option.label}</span>
        </label>
      ))}
    </div>
  );
}

function ListEditor(props: {
  field: Extract<Field, { kind: "text-list" }>;
  value: FieldValue;
  onChange: (value: FieldValue) => void;
  common: Record<string, unknown>;
}) {
  const { field, onChange } = props;
  const items = props.value.kind === "list" ? props.value.items : [];
  const full = field.maxItems !== undefined && items.length >= field.maxItems;
  const set = (next: string[]) => onChange({ kind: "list", items: next });
  const listRef = useRef<HTMLOListElement>(null);
  const shown = useRef(items.length);
  useEffect(() => {
    // A newly added item takes focus, so typing can continue from the keyboard.
    if (items.length > shown.current) {
      listRef.current?.querySelector<HTMLInputElement>("li:last-child input")?.focus();
    }
    shown.current = items.length;
  }, [items.length]);
  return (
    <div className="list-editor" role="group" {...props.common}>
      {items.length === 0 ? <p className="list-empty">No items.</p> : null}
      <ol ref={listRef}>
        {items.map((item, index) => (
          <li key={index}>
            <input
              id={index === 0 ? `q-${field.id}` : undefined}
              aria-label={`${field.title} ${index + 1}`}
              type="text"
              autoComplete="off"
              value={item}
              onChange={(e) => set(items.map((v, i) => (i === index ? e.target.value : v)))}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  if (!full) set([...items, ""]);
                }
              }}
            />
            <button
              type="button"
              className="ghost"
              aria-label={`Remove ${field.title} ${index + 1}`}
              onClick={() => set(items.filter((_, i) => i !== index))}
            >
              Remove
            </button>
          </li>
        ))}
      </ol>
      <button
        type="button"
        id={items.length === 0 ? `q-${field.id}` : undefined}
        className="ghost add"
        disabled={full}
        onClick={() => set([...items, ""])}
      >
        Add {field.title.toLowerCase()}
      </button>
    </div>
  );
}
