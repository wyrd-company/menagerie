import { describe, expect, it } from "vitest";
import type { BatchDocument } from "../api/types.ts";
import batch1 from "../../test/fixtures/toha/documents/create-batch-1.json";
import upgradeRaw from "../../test/fixtures/toha/documents/upgrade-batch-1.json";
import { readField, readFields } from "./fields.ts";

const byId = (doc: unknown) =>
  Object.fromEntries(readFields(doc as BatchDocument).map((f) => [f.id, f]));

describe("readFields on a producer batch", () => {
  const fields = byId(batch1);

  it("reads every property of the batch, none dropped", () => {
    expect(Object.keys(fields).sort()).toEqual(Object.keys(batch1.schema.properties).sort());
    expect(Object.keys(fields)).toHaveLength(10);
  });

  it("maps each x-toha-type to its control", () => {
    expect(Object.fromEntries(Object.entries(fields).map(([id, f]) => [id, f.kind]))).toEqual({
      enabled: "confirm",
      features: "multiselect",
      labels: "text-list",
      notes: "multiline",
      palette: "select",
      remote: "git",
      settings: "document",
      size: "select",
      title: "text",
      verbose: "confirm",
    });
  });

  it("keeps required, null, default and constraint facts", () => {
    expect(fields.title).toMatchObject({
      required: true,
      nullable: false,
      hasDefault: false,
      placeholder: "Sample project",
      rules: { minLength: 3, maxLength: 40 },
    });
    expect(fields.enabled).toMatchObject({
      required: true,
      nullable: false,
      hasDefault: true,
      default: true,
    });
    expect(fields.verbose).toMatchObject({ required: false, nullable: true, hasDefault: false });
    expect(fields.palette).toMatchObject({ nullable: true, options: ["red", "green", "blue"] });
    expect(fields.size).toMatchObject({
      nullable: false,
      options: ["small", "medium", "large", "halt"],
      default: "medium",
    });
    expect(fields.features).toMatchObject({
      minItems: 1,
      maxItems: 3,
      default: ["alpha"],
      nullable: true,
    });
    expect(fields.labels).toMatchObject({
      minItems: 0,
      maxItems: 3,
      item: { pattern: "^[a-z0-9-]+$" },
    });
    expect(fields.settings).toMatchObject({
      required: true,
      schemas: ["https://schemas.example.test/sample-settings/1"],
      input: "settings.json",
    });
  });
});

describe("readFields on the raw producer upgrade batch", () => {
  it("renders the whole received schema, including questions with recorded defaults", () => {
    const fields = byId(upgradeRaw);
    expect(Object.keys(fields).sort()).toEqual(Object.keys(upgradeRaw.schema.properties).sort());
    expect(Object.keys(fields)).toHaveLength(11);
    expect(fields.region).toMatchObject({
      kind: "select",
      required: true,
      options: ["north", "south"],
    });
    expect(fields.labels).toMatchObject({ kind: "text-list", default: ["one", "two"] });
  });
});

describe("readField fallbacks", () => {
  it("answers an unknown x-toha-type as raw JSON instead of dropping it", () => {
    expect(readField("odd", { type: "integer", "x-toha-type": "number" }, true)).toMatchObject({
      kind: "json",
      tohaType: "number",
      title: "odd",
    });
  });

  it("answers a property without x-toha-type as raw JSON", () => {
    expect(readField("plain", { type: "string" }, false).kind).toBe("json");
  });

  it("falls back when a declared type does not fit its JSON type", () => {
    expect(readField("c", { type: "string", "x-toha-type": "confirm" }, false).kind).toBe("json");
    expect(readField("s", { type: "string", "x-toha-type": "select" }, false).kind).toBe("json");
  });

  it("reads a nullable boolean from anyOf null", () => {
    expect(
      readField(
        "x",
        { type: "boolean", anyOf: [{ type: "null" }], "x-toha-type": "confirm" },
        false,
      ).nullable,
    ).toBe(true);
  });
});
