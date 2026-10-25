// Reads a Toha question batch schema into typed fields. Every property in the
// received schema becomes a field; a property this reader does not recognise
// becomes a raw JSON field, so no question is ever dropped.
import { rawPath, topLevelMembers } from "../api/rawJson.ts";
import type { BatchDocument, PropertySchema } from "../api/types.ts";

interface Base {
  id: string;
  title: string;
  description?: string;
  placeholder?: string;
  required: boolean;
  /** The schema admits an explicit null answer. */
  nullable: boolean;
  hasDefault: boolean;
  default?: unknown;
  /** The default as the producer wrote it, when the document's text is known. */
  defaultText?: string;
  /** The question type Toha declared in x-toha-type, verbatim. */
  tohaType: string;
}

export interface TextRules {
  minLength?: number;
  maxLength?: number;
  pattern?: string;
}

export type Field =
  | (Base & { kind: "text" | "git" | "multiline"; rules: TextRules })
  | (Base & { kind: "text-list"; item: TextRules; minItems?: number; maxItems?: number })
  | (Base & { kind: "confirm" })
  | (Base & { kind: "select"; options: string[] })
  | (Base & { kind: "multiselect"; options: string[]; minItems?: number; maxItems?: number })
  | (Base & { kind: "document"; schemas: string[]; input?: string })
  | (Base & { kind: "json"; schema: PropertySchema });

export type FieldKind = Field["kind"];

function types(schema: PropertySchema): string[] {
  if (Array.isArray(schema.type)) return schema.type;
  return schema.type === undefined ? [] : [schema.type];
}

function textRules(schema: PropertySchema | undefined): TextRules {
  const rules: TextRules = {};
  if (typeof schema?.minLength === "number") rules.minLength = schema.minLength;
  if (typeof schema?.maxLength === "number") rules.maxLength = schema.maxLength;
  if (typeof schema?.pattern === "string") rules.pattern = schema.pattern;
  return rules;
}

function stringOptions(values: unknown[] | undefined): string[] | null {
  if (!Array.isArray(values) || !values.every((v) => typeof v === "string")) return null;
  return values as string[];
}

/** Select options sit in `enum`, or in `anyOf: [{enum}, {type: null}]` when not required. */
function selectOptions(schema: PropertySchema): string[] | null {
  const direct = stringOptions(schema.enum);
  if (direct) return direct;
  for (const branch of schema.anyOf ?? []) {
    const options = stringOptions(branch.enum);
    if (options) return options;
  }
  return null;
}

function itemBounds(schema: PropertySchema): { minItems?: number; maxItems?: number } {
  const bounds: { minItems?: number; maxItems?: number } = {};
  if (typeof schema.minItems === "number") bounds.minItems = schema.minItems;
  if (typeof schema.maxItems === "number") bounds.maxItems = schema.maxItems;
  return bounds;
}

export function readField(
  id: string,
  schema: PropertySchema,
  required: boolean,
  defaultText?: string,
): Field {
  const t = types(schema);
  const base: Base = {
    id,
    title: typeof schema.title === "string" && schema.title !== "" ? schema.title : id,
    required,
    nullable: t.includes("null") || (schema.anyOf ?? []).some((b) => b.type === "null"),
    hasDefault: Object.hasOwn(schema, "default"),
    tohaType: typeof schema["x-toha-type"] === "string" ? schema["x-toha-type"] : "",
  };
  if (typeof schema.description === "string") base.description = schema.description;
  if (base.hasDefault) base.default = schema.default;
  if (base.hasDefault && defaultText !== undefined) base.defaultText = defaultText;
  const example = schema.examples?.[0];
  if (typeof example === "string") base.placeholder = example;

  const json = (): Field => ({ ...base, kind: "json", schema });
  switch (base.tohaType) {
    case "text":
      if (t.includes("array")) {
        return { ...base, kind: "text-list", item: textRules(schema.items), ...itemBounds(schema) };
      }
      return t.includes("string") ? { ...base, kind: "text", rules: textRules(schema) } : json();
    case "git":
    case "multiline":
      return t.includes("string")
        ? { ...base, kind: base.tohaType, rules: textRules(schema) }
        : json();
    case "confirm":
      return t.includes("boolean") ? { ...base, kind: "confirm" } : json();
    case "select": {
      const options = selectOptions(schema);
      return options && t.includes("string") ? { ...base, kind: "select", options } : json();
    }
    case "multiselect": {
      const options = schema.items ? selectOptions(schema.items) : null;
      return options && t.includes("array")
        ? { ...base, kind: "multiselect", options, ...itemBounds(schema) }
        : json();
    }
    case "document": {
      const schemas = stringOptions(schema["x-toha-schemas"]) ?? [];
      const field: Field = { ...base, kind: "document", schemas };
      if (typeof schema["x-toha-input"] === "string") field.input = schema["x-toha-input"];
      return field;
    }
    default:
      return json();
  }
}

/**
 * The batch's fields, in the order the schema lists its properties. With the
 * document's text, each default also keeps the producer's own JSON text.
 */
export function readFields(document: BatchDocument, documentText?: string | null): Field[] {
  const required = new Set(document.schema.required ?? []);
  const rawProperties = documentText ? rawPath(documentText, ["schema", "properties"]) : undefined;
  const rawDefaults = rawProperties ? topLevelMembers(rawProperties) : new Map<string, string>();
  return Object.entries(document.schema.properties ?? {}).map(([id, schema]) => {
    const rawProperty = rawDefaults.get(id);
    const defaultText = rawProperty ? rawPath(rawProperty, ["default"]) : undefined;
    return readField(id, schema, required.has(id), defaultText);
  });
}
