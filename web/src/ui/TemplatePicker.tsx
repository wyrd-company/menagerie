import { useEffect, useState } from "react";
import { ApiError, listTemplates } from "../api/client.ts";
import type { Template } from "../api/types.ts";

interface Props {
  value: string | null;
  onChange: (id: string) => void;
  legend: string;
}

/** The approved template list as a set of choice cards. */
export function TemplatePicker({ value, onChange, legend }: Props) {
  const [templates, setTemplates] = useState<Template[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    listTemplates()
      .then(setTemplates)
      .catch((e: unknown) => setError(e instanceof ApiError ? e.message : String(e)));
  }, []);

  return (
    <fieldset className="field template-picker">
      <legend className="field-label">
        <span className="field-title">{legend}</span>
        <span className="required" aria-label="required">
          ✱
        </span>
      </legend>
      {error ? (
        <p className="field-errors" role="alert">
          {error}
        </p>
      ) : null}
      {templates === null && !error ? (
        <p className="muted">Reading the approved templates…</p>
      ) : null}
      {templates?.length === 0 ? (
        <p className="muted">No templates are approved in the configuration.</p>
      ) : null}
      <div className="template-cards" role="radiogroup">
        {templates?.map((t) => (
          <label key={t.id} className={`template-card${value === t.id ? " is-selected" : ""}`}>
            <input
              type="radio"
              name="template"
              value={t.id}
              checked={value === t.id}
              onChange={() => onChange(t.id)}
            />
            <span className="template-name">{t.name}</span>
            <span className="template-id mono">{t.id}</span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}
