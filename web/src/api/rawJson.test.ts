import { describe, expect, it } from "vitest";
import { objectFromRaw, rawMember, topLevelMembers } from "./rawJson.ts";

// Values JSON.parse + JSON.stringify would change: precision, 1.0, -0.0, escapes.
const session = `{"counter":12345678901234567890,"ratio":1.0,"offset":-0.0,"path":"a\\/b","nested":{"s":"}{\\"]","list":[1e400,{"x":[]}]}}`;

describe("topLevelMembers", () => {
  it("returns each member's raw text unchanged", () => {
    const text = ` { "session" : ${session} , "state":"questions","document":null,"warnings":[ ] }`;
    const members = topLevelMembers(text);
    expect([...members.keys()]).toEqual(["session", "state", "document", "warnings"]);
    expect(members.get("session")).toBe(session);
    expect(members.get("state")).toBe('"questions"');
    expect(members.get("document")).toBe("null");
    expect(members.get("warnings")).toBe("[ ]");
  });

  it("shows why a parse round trip is not used: it changes the session", () => {
    expect(JSON.stringify(JSON.parse(session))).not.toBe(session);
  });

  it("decodes escaped member names", () => {
    expect(topLevelMembers('{"a\\u0062":1}').get("ab")).toBe("1");
  });

  it("handles an empty object", () => {
    expect(topLevelMembers("{}").size).toBe(0);
  });

  it.each([
    ["not an object", "[1]"],
    ["unterminated string", '{"a":"b}'],
    ["missing colon", '{"a" 1}'],
    ["unterminated container", '{"a":{"b":1}'],
    ["trailing data", '{"a":1} x'],
    ["bad literal", '{"a":tru}'],
    ["missing value", '{"a":}'],
  ])("rejects %s", (_, text) => {
    expect(() => topLevelMembers(text)).toThrow(SyntaxError);
  });
});

describe("rawMember and objectFromRaw", () => {
  it("rebuilds a body that carries the session byte for byte", () => {
    const reply = `{"session":${session},"state":"questions"}`;
    const raw = rawMember(reply, "session");
    expect(raw).toBe(session);
    const body = objectFromRaw([
      ["session", raw as string],
      ["submission", JSON.stringify({ template: "t", answers: {} })],
    ]);
    expect(body).toBe(`{"session":${session},"submission":{"template":"t","answers":{}}}`);
    expect(() => JSON.parse(body)).not.toThrow();
  });

  it("returns undefined for an absent member", () => {
    expect(rawMember('{"a":1}', "session")).toBeUndefined();
  });
});
