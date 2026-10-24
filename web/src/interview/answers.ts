// Form state for one batch and the answers document built from it.
//
// Toha gives a question its default, or its empty answer, when the answers
// document leaves it out, and treats an explicit null as an empty answer that
// does not take the default. So the form sends only the questions the person
// changed; an untouched question is left out and Toha applies its own default.
import type { Submission } from "../api/types.ts";
import type { Field, TextRules } from "./fields.ts";

export type Choice = string | null | undefined;

/** The editable value of one field. `undefined` choice means nothing chosen yet. */
export type FieldValue =
  | { kind: "text"; text: string }
  | { kind: "list"; items: string[] }
  | { kind: "choice"; choice: Choice }
  | { kind: "flag"; flag: boolean | null | undefined }
  | { kind: "set"; values: string[] }
  | { kind: "json"; text: string };

export interface FieldState {
  value: FieldValue;
  dirty: boolean;
}

export type FormState = Record<string, FieldState>;

function pretty(value: unknown): string {
  return JSON.stringify(value, null, 2);
}

export function initialValue(field: Field): FieldValue {
  const d = field.default;
  switch (field.kind) {
    case "text":
    case "git":
    case "multiline":
      return { kind: "text", text: typeof d === "string" ? d : "" };
    case "text-list":
      return { kind: "list", items: Array.isArray(d) ? d.map(String) : [] };
    case "confirm":
      return { kind: "flag", flag: typeof d === "boolean" ? d : undefined };
    case "select":
      return { kind: "choice", choice: typeof d === "string" ? d : undefined };
    case "multiselect":
      return { kind: "set", values: Array.isArray(d) ? d.map(String) : [] };
    case "document":
      if (field.hasDefault && d !== null) return { kind: "json", text: pretty(d) };
      return {
        kind: "json",
        text: field.schemas.length === 1 ? pretty({ $schema: field.schemas[0] }) : "",
      };
    case "json":
      return { kind: "json", text: field.hasDefault ? pretty(d) : "" };
  }
}

export function initialForm(fields: Field[]): FormState {
  return Object.fromEntries(fields.map((f) => [f.id, { value: initialValue(f), dirty: false }]));
}

/** A value the browser cannot turn into an answer, with the reason. */
export class AnswerError extends Error {}

/** The answer a changed field sends. */
export function answerFor(field: Field, value: FieldValue): unknown {
  switch (value.kind) {
    case "text":
      // An emptied optional field is "no answer"; a required one sends "" for Toha to judge.
      return value.text === "" && field.nullable && !field.required ? null : value.text;
    case "list":
      return value.items.filter((item) => item !== "");
    case "choice":
      return value.choice ?? null;
    case "flag":
      return value.flag ?? null;
    case "set":
      return value.values;
    case "json": {
      if (value.text.trim() === "") {
        if (field.nullable || !field.required) return null;
        throw new AnswerError("Enter a JSON value.");
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(value.text);
      } catch (error) {
        throw new AnswerError(`Not valid JSON: ${(error as Error).message}`);
      }
      if (
        field.kind === "document" &&
        (parsed === null || typeof parsed !== "object" || Array.isArray(parsed))
      ) {
        throw new AnswerError("A document answer is one JSON object.");
      }
      return parsed;
    }
  }
}

function codePoints(text: string): number {
  return [...text].length;
}

function textProblems(text: string, rules: TextRules): string[] {
  const problems: string[] = [];
  const n = codePoints(text);
  if (rules.minLength !== undefined && n < rules.minLength) {
    problems.push(`must be at least ${rules.minLength} characters`);
  }
  if (rules.maxLength !== undefined && n > rules.maxLength) {
    problems.push(`must be at most ${rules.maxLength} characters`);
  }
  return problems;
}

function countProblems(count: number, min?: number, max?: number): string[] {
  const problems: string[] = [];
  if (min !== undefined && count < min)
    problems.push(`must have at least ${min} item${min === 1 ? "" : "s"}`);
  if (max !== undefined && count > max)
    problems.push(`must have at most ${max} item${max === 1 ? "" : "s"}`);
  return problems;
}

/**
 * Checks that the browser can decide without Toha: JSON syntax, an unanswered
 * required question, character and item bounds. Patterns are left to Toha,
 * whose regular expressions are not JavaScript's.
 */
export function problemsFor(field: Field, state: FieldState): string[] {
  const { value, dirty } = state;
  const unanswered =
    (value.kind === "text" && value.text === "") ||
    (value.kind === "choice" && (value.choice === undefined || value.choice === null)) ||
    (value.kind === "flag" && (value.flag === undefined || value.flag === null)) ||
    (value.kind === "json" && value.text.trim() === "");
  // An untouched document is left out so Toha can read its input file from the target.
  const tohaDecides = field.hasDefault || field.kind === "document";
  if (field.required && unanswered && (dirty || !tohaDecides)) return ["is required"];
  if (!dirty) return [];
  try {
    answerFor(field, value);
  } catch (error) {
    if (error instanceof AnswerError) return [error.message];
    throw error;
  }
  switch (field.kind) {
    case "text":
    case "git":
    case "multiline":
      return value.kind === "text" && value.text !== ""
        ? textProblems(value.text, field.rules)
        : [];
    case "text-list": {
      if (value.kind !== "list") return [];
      const items = value.items.filter((i) => i !== "");
      return [
        ...countProblems(items.length, field.minItems, field.maxItems),
        ...items.flatMap((item) => textProblems(item, field.item).map((p) => `each item ${p}`)),
      ].filter((p, i, all) => all.indexOf(p) === i);
    }
    case "multiselect":
      return value.kind === "set"
        ? countProblems(value.values.length, field.minItems, field.maxItems)
        : [];
    default:
      return [];
  }
}

/** The answers document for the changed fields of this batch. */
export function buildSubmission(template: string, fields: Field[], form: FormState): Submission {
  const answers: Record<string, unknown> = {};
  for (const field of fields) {
    const state = form[field.id];
    if (state?.dirty) answers[field.id] = answerFor(field, state.value);
  }
  return { template, answers };
}
