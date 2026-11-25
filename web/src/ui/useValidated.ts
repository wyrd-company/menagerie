import { useState } from "react";
import { ApiError } from "../api/client.ts";

const asError = (e: unknown) => (e instanceof ApiError ? e : new ApiError(0, null, String(e)));

/** A Validate's or Save's reply, with the values its request sent. */
interface Reply<T> {
  values: string;
  validated?: T;
  error?: ApiError;
}

/**
 * Validate, then save: Validate runs the checks alone and stores nothing; Save
 * stays disabled until a Validate of the current values passes, and checks
 * again. Any edit disables Save and clears the result and refusal. values
 * names what the form would send now; a reply counts only while the form
 * still holds the values its request sent, so a Validate that an edit
 * overtook enables nothing.
 */
export function useValidated<T>(values: string) {
  const [reply, setReply] = useState<Reply<T> | null>(null);
  const [busy, setBusy] = useState<"validating" | "saving" | null>(null);
  const current = reply?.values === values ? reply : null;
  const edited = () => setReply(null);
  const run = async (kind: "validating" | "saving", call: () => Promise<T>) => {
    const sent = values;
    setBusy(kind);
    setReply((last) => (last?.error ? null : last));
    try {
      const value = await call();
      if (kind === "validating") setReply({ values: sent, validated: value });
      return value;
    } catch (e) {
      setReply({ values: sent, error: asError(e) });
      return null;
    } finally {
      setBusy(null);
    }
  };
  return {
    error: current?.error ?? null,
    validated: current?.validated ?? null,
    busy,
    edited,
    run,
  };
}
