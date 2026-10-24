import { describe, expect, it } from "vitest";
import type { BatchDocument } from "../api/types.ts";
import batch1 from "../../test/fixtures/toha/documents/create-batch-1.json";
import {
  AnswerError,
  answerFor,
  buildSubmission,
  initialForm,
  problemsFor,
  type FieldValue,
  type FormState,
} from "./answers.ts";
import { readField, readFields, type Field } from "./fields.ts";

const doc = batch1 as unknown as BatchDocument;
const fields = readFields(doc);
const field = (id: string) => fields.find((f) => f.id === id) as Field;
const edit = (form: FormState, id: string, value: FieldValue): FormState => ({
  ...form,
  [id]: { value, dirty: true },
});

describe("initialForm", () => {
  it("starts from each question's default, untouched", () => {
    const form = initialForm(fields);
    expect(form.enabled).toEqual({ value: { kind: "flag", flag: true }, dirty: false });
    expect(form.verbose?.value).toEqual({ kind: "flag", flag: undefined });
    expect(form.size?.value).toEqual({ kind: "choice", choice: "medium" });
    expect(form.palette?.value).toEqual({ kind: "choice", choice: undefined });
    expect(form.features?.value).toEqual({ kind: "set", values: ["alpha"] });
    expect(form.labels?.value).toEqual({ kind: "list", items: [] });
    expect(form.settings?.value).toEqual({
      kind: "json",
      text: '{\n  "$schema": "https://schemas.example.test/sample-settings/1"\n}',
    });
  });
});

describe("buildSubmission", () => {
  it("wraps answers in the identity envelope with the batch's template", () => {
    const submission = buildSubmission(doc.context.template, fields, initialForm(fields));
    expect(submission).toEqual({ template: doc.context.template, answers: {} });
  });

  it("leaves untouched questions out so Toha applies their defaults", () => {
    let form = initialForm(fields);
    form = edit(form, "title", { kind: "text", text: "Sample project" });
    expect(buildSubmission("t", fields, form).answers).toEqual({ title: "Sample project" });
  });

  it("sends typed values for every kind of question", () => {
    let form = initialForm(fields);
    form = edit(form, "title", { kind: "text", text: "Sample project" });
    form = edit(form, "notes", { kind: "text", text: "line one\nline two" });
    form = edit(form, "remote", { kind: "text", text: "https://example.test/sample.git" });
    form = edit(form, "enabled", { kind: "flag", flag: false });
    form = edit(form, "verbose", { kind: "flag", flag: null });
    form = edit(form, "size", { kind: "choice", choice: "large" });
    form = edit(form, "palette", { kind: "choice", choice: null });
    form = edit(form, "features", { kind: "set", values: ["alpha", "gamma"] });
    form = edit(form, "labels", { kind: "list", items: ["one", "", "two"] });
    form = edit(form, "settings", {
      kind: "json",
      text: '{"$schema":"https://schemas.example.test/sample-settings/1","name":"sample","count":2}',
    });
    expect(buildSubmission("t", fields, form).answers).toEqual({
      title: "Sample project",
      notes: "line one\nline two",
      remote: "https://example.test/sample.git",
      enabled: false,
      verbose: null,
      size: "large",
      palette: null,
      features: ["alpha", "gamma"],
      labels: ["one", "two"],
      settings: {
        $schema: "https://schemas.example.test/sample-settings/1",
        name: "sample",
        count: 2,
      },
    });
  });
});

describe("answerFor", () => {
  it("sends an emptied optional text as null, an emptied required text as an empty string", () => {
    expect(answerFor(field("notes"), { kind: "text", text: "" })).toBeNull();
    expect(answerFor(field("title"), { kind: "text", text: "" })).toBe("");
  });

  it("refuses a document that is not one JSON object", () => {
    expect(() => answerFor(field("settings"), { kind: "json", text: "[1]" })).toThrow(AnswerError);
    expect(() => answerFor(field("settings"), { kind: "json", text: "{" })).toThrow(
      /Not valid JSON/,
    );
  });

  it("parses a raw JSON answer for an unknown question type", () => {
    const odd = readField("odd", { type: "integer", "x-toha-type": "number" }, true);
    expect(answerFor(odd, { kind: "json", text: "42" })).toBe(42);
    expect(() => answerFor(odd, { kind: "json", text: "" })).toThrow(AnswerError);
  });
});

describe("problemsFor", () => {
  const state = (value: FieldValue, dirty = true) => ({ value, dirty });

  it("reports an unanswered required question without a default", () => {
    expect(problemsFor(field("title"), state({ kind: "text", text: "" }, false))).toEqual([
      "is required",
    ]);
    expect(problemsFor(field("size"), state({ kind: "choice", choice: "medium" }, false))).toEqual(
      [],
    );
  });

  it("leaves an untouched required document to Toha, which may read its input file", () => {
    expect(problemsFor(field("settings"), state({ kind: "json", text: "" }, false))).toEqual([]);
    expect(problemsFor(field("settings"), state({ kind: "json", text: "" }))).toEqual([
      "is required",
    ]);
  });

  it("counts characters as code points", () => {
    expect(problemsFor(field("title"), state({ kind: "text", text: "🦊🦊" }))).toEqual([
      "must be at least 3 characters",
    ]);
    expect(problemsFor(field("title"), state({ kind: "text", text: "🦊🦊🦊" }))).toEqual([]);
    expect(problemsFor(field("title"), state({ kind: "text", text: "x".repeat(41) }))).toEqual([
      "must be at most 40 characters",
    ]);
  });

  it("checks item counts", () => {
    expect(problemsFor(field("features"), state({ kind: "set", values: [] }))).toEqual([
      "must have at least 1 item",
    ]);
    expect(
      problemsFor(field("labels"), state({ kind: "list", items: ["a", "b", "c", "d"] })),
    ).toEqual(["must have at most 3 items"]);
  });

  it("leaves patterns to Toha", () => {
    expect(problemsFor(field("labels"), state({ kind: "list", items: ["Bad Label"] }))).toEqual([]);
  });

  it("reports unparseable JSON", () => {
    expect(problemsFor(field("settings"), state({ kind: "json", text: "{" }))[0]).toMatch(
      /Not valid JSON/,
    );
  });
});
