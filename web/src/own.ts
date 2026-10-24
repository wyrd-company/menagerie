/**
 * Reads a record member only when the record itself holds it. Question ids,
 * workspace names and states come from outside the browser, and a name such
 * as "constructor" or "__proto__" must not reach an inherited value.
 */
export function own<T>(
  record: Readonly<Record<string, T>> | undefined,
  key: string,
): T | undefined {
  return record !== undefined && Object.hasOwn(record, key) ? record[key] : undefined;
}
