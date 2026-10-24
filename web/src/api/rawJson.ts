// The staged session is opaque to the browser: it must go back to the server
// exactly as it arrived, so its numbers and member order never pass through
// JSON.parse and JSON.stringify. These helpers find the raw text of a member.

/** Raw JSON text that the browser carries without decoding. */
export type RawJson = string & { readonly __raw: unique symbol };

/** Returns the raw text of each top-level member of a JSON object. */
export function topLevelMembers(text: string): Map<string, string> {
  const members = new Map<string, string>();
  let i = skipSpace(text, 0);
  if (text[i] !== "{") throw new SyntaxError("expected a JSON object");
  i = skipSpace(text, i + 1);
  if (text[i] === "}") return members;
  for (;;) {
    if (text[i] !== '"') throw new SyntaxError(`expected a member name at ${i}`);
    const nameEnd = scanString(text, i);
    const name = JSON.parse(text.slice(i, nameEnd)) as string;
    i = skipSpace(text, nameEnd);
    if (text[i] !== ":") throw new SyntaxError(`expected ':' at ${i}`);
    const valueStart = skipSpace(text, i + 1);
    const valueEnd = scanValue(text, valueStart);
    members.set(name, text.slice(valueStart, valueEnd));
    i = skipSpace(text, valueEnd);
    if (text[i] === ",") {
      i = skipSpace(text, i + 1);
      continue;
    }
    if (text[i] === "}") break;
    throw new SyntaxError(`expected ',' or '}' at ${i}`);
  }
  if (skipSpace(text, i + 1) !== text.length) throw new SyntaxError("trailing data after object");
  return members;
}

/** Returns the raw text of one top-level member, or undefined when absent. */
export function rawMember(text: string, name: string): RawJson | undefined {
  return topLevelMembers(text).get(name) as RawJson | undefined;
}

/** Builds a JSON object from raw member text, in the given order. */
export function objectFromRaw(members: [string, string][]): string {
  return `{${members.map(([name, raw]) => `${JSON.stringify(name)}:${raw}`).join(",")}}`;
}

function skipSpace(text: string, i: number): number {
  while (i < text.length && " \t\n\r".includes(text[i] as string)) i++;
  return i;
}

function scanString(text: string, i: number): number {
  for (let j = i + 1; j < text.length; j++) {
    const c = text[j];
    if (c === "\\") j++;
    else if (c === '"') return j + 1;
  }
  throw new SyntaxError("unterminated string");
}

function scanValue(text: string, i: number): number {
  const c = text[i];
  if (c === '"') return scanString(text, i);
  if (c === "{" || c === "[") {
    let depth = 0;
    for (let j = i; j < text.length; j++) {
      const d = text[j];
      if (d === '"') j = scanString(text, j) - 1;
      else if (d === "{" || d === "[") depth++;
      else if (d === "}" || d === "]") {
        depth--;
        if (depth === 0) return j + 1;
      }
    }
    throw new SyntaxError("unterminated container");
  }
  let j = i;
  while (j < text.length && !",}] \t\n\r".includes(text[j] as string)) j++;
  if (j === i) throw new SyntaxError(`expected a value at ${i}`);
  // Validates the literal (number, true, false, null) without keeping the decoded value.
  JSON.parse(text.slice(i, j));
  return j;
}
