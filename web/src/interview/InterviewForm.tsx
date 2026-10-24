import { useEffect, useMemo, useRef, useState, type FormEvent, type MouseEvent } from "react";
import type { BatchDocument, Submission } from "../api/types.ts";
import { own } from "../own.ts";
import { buildSubmission, initialForm, problemsFor, type FormState } from "./answers.ts";
import { FieldInput } from "./FieldInput.tsx";
import { readFields } from "./fields.ts";
import { Messages } from "./Messages.tsx";

interface Props {
  document: BatchDocument;
  /** The document's text as received, so JSON defaults keep the producer's digits. */
  documentText?: string | null;
  /** A batch number shown to the person; it counts batches seen in this job. */
  ordinal: number;
  submitting: boolean;
  submitError: string | null;
  onSubmit: (submission: Submission) => void;
}

function propertyKey(document: BatchDocument): string {
  return JSON.stringify(Object.keys(document.schema.properties ?? {}));
}

/**
 * Renders a whole Toha question batch. A rejected submission comes back as the
 * same batch with errors; the person's answers stay in place so they can fix
 * them. Any other batch starts from its own defaults.
 */
export function InterviewForm(props: Props) {
  const { document, documentText, ordinal, submitting, submitError, onSubmit } = props;
  const fields = useMemo(() => readFields(document, documentText), [document, documentText]);
  const [form, setForm] = useState<FormState>(() => initialForm(fields));
  const [shownDocument, setShownDocument] = useState(document);
  const [checked, setChecked] = useState(false);
  // Answers changed since this document arrived; their server errors are stale.
  const [edited, setEdited] = useState<ReadonlySet<string>>(new Set());
  const summaryRef = useRef<HTMLDivElement>(null);

  if (shownDocument !== document) {
    const rejectedSameBatch =
      document.errors !== undefined && propertyKey(document) === propertyKey(shownDocument);
    setShownDocument(document);
    setEdited(new Set());
    if (!rejectedSameBatch) {
      setForm(initialForm(fields));
      setChecked(false);
    }
  }

  const serverErrors = document.errors ?? {};
  const fieldIds = new Set(fields.map((f) => f.id));
  const strayErrors = Object.entries(serverErrors).filter(([id]) => !fieldIds.has(id));
  const localProblems = Object.fromEntries(
    fields.map((f) => [
      f.id,
      problemsFor(f, own(form, f.id) ?? { value: { kind: "text", text: "" }, dirty: false }),
    ]),
  );
  const shownErrors = (id: string): string[] => {
    const local = checked ? (own(localProblems, id) ?? []) : [];
    // A server error stays until the person changes that answer.
    const server = edited.has(id) ? [] : (own(serverErrors, id) ?? []);
    return [...new Set([...server, ...local])];
  };
  const errorCount = fields.filter((f) => shownErrors(f.id).length > 0).length + strayErrors.length;

  // The error summary takes focus when Toha rejects answers or the browser blocks a submission.
  const [blockedAttempts, setBlockedAttempts] = useState(0);
  useEffect(() => {
    if (document.errors && Object.keys(document.errors).length > 0) summaryRef.current?.focus();
  }, [document]);
  useEffect(() => {
    if (blockedAttempts > 0) summaryRef.current?.focus();
  }, [blockedAttempts]);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    // One answers request at a time: the session it carries is only valid once.
    if (submitting) return;
    setChecked(true);
    const blocking = fields.filter((f) => (own(localProblems, f.id) ?? []).length > 0);
    if (blocking.length > 0) {
      setBlockedAttempts((n) => n + 1);
      return;
    }
    onSubmit(buildSubmission(document.context.template, fields, form));
  };

  return (
    <form
      className="interview"
      noValidate
      onSubmit={submit}
      onKeyDown={(e) => {
        if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) submit(e);
      }}
      aria-busy={submitting}
    >
      <header className="interview-head">
        <p className="eyebrow">Question batch {ordinal}</p>
        <p className="interview-template mono" title="Template">
          {document.context.template}
          {document.context.commit ? <span> @ {document.context.commit.slice(0, 12)}</span> : null}
        </p>
      </header>
      <Messages messages={document.messages} />
      {errorCount > 0 ? (
        <div className="error-summary" ref={summaryRef} tabIndex={-1} role="alert">
          <p>{errorCount === 1 ? "One answer needs" : `${errorCount} answers need`} attention.</p>
          <ul>
            {fields
              .filter((f) => shownErrors(f.id).length > 0)
              .map((f) => (
                <li key={f.id}>
                  <a href={`#q-${f.id}`} onClick={(e) => focusQuestion(e, f.id)}>
                    {f.title}
                  </a>
                  : {shownErrors(f.id).join("; ")}
                </li>
              ))}
            {strayErrors.map(([id, list]) => (
              <li key={id}>
                <span className="mono">{id}</span>: {list.join("; ")}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
      <div className="fields">
        {fields.map((field) => (
          <FieldInput
            key={field.id}
            field={field}
            value={(own(form, field.id) ?? { value: { kind: "text", text: "" } }).value}
            errors={shownErrors(field.id)}
            onChange={(value) => {
              setForm((current) => ({ ...current, [field.id]: { value, dirty: true } }));
              setEdited((current) =>
                current.has(field.id) ? current : new Set([...current, field.id]),
              );
            }}
          />
        ))}
        {fields.length === 0 ? <p className="muted">This batch has no questions.</p> : null}
      </div>
      {submitError ? (
        <p className="submit-error" role="alert">
          {submitError}
        </p>
      ) : null}
      <div className="interview-actions">
        <button type="submit" className="primary" disabled={submitting}>
          {submitting ? "Sending answers…" : "Send answers"}
        </button>
        <span className="keyhint">
          <kbd>Ctrl</kbd> + <kbd>Enter</kbd>
        </span>
      </div>
    </form>
  );
}

/** Moves focus to a question's first control from an error-summary link. */
function focusQuestion(event: MouseEvent<HTMLAnchorElement>, id: string) {
  const target = globalThis.document.getElementById(`q-${id}`);
  if (!target) return;
  event.preventDefault();
  target.focus();
  target.scrollIntoView?.({ block: "center" });
}
