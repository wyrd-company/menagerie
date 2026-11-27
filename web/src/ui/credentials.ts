import { useEffect, useState } from "react";
import { ApiError, listCredentials, readTemplateAddress } from "../api/client.ts";
import type { Credentials } from "../api/types.ts";

const message = (e: unknown) => (e instanceof ApiError ? e.message : String(e));

/** The credentials a backup repository or a template can name, read once for a screen. */
export function useCredentials() {
  const [credentials, setCredentials] = useState<Credentials | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    listCredentials().then(setCredentials, (e: unknown) => setError(message(e)));
  }, []);
  return { credentials, error };
}

/** How long typing pauses before an address is read. */
const READ_AFTER_MS = 300;

/**
 * The Git URL the template service reads from address, the URL a
 * template's credential has to fit: "" while none is read, with why the
 * address could not be read, if it could not, and whether a reading of
 * this address is still to come.
 */
export function useTemplateURL(address: string): {
  url: string;
  error: string | null;
  pending: boolean;
} {
  const [read, setRead] = useState<{ address: string; url: string; error: string | null }>({
    address: "",
    url: "",
    error: null,
  });
  const wanted = address.trim();
  useEffect(() => {
    if (wanted === "") return;
    let current = true;
    const timer = setTimeout(() => {
      readTemplateAddress(wanted).then(
        (url) => current && setRead({ address: wanted, url, error: null }),
        (e: unknown) => current && setRead({ address: wanted, url: "", error: message(e) }),
      );
    }, READ_AFTER_MS);
    return () => {
      current = false;
      clearTimeout(timer);
    };
  }, [wanted]);
  // A reading of another address is no reading of this one.
  if (wanted === "") return { url: "", error: null, pending: false };
  return read.address === wanted
    ? { url: read.url, error: read.error, pending: false }
    : { url: "", error: null, pending: true };
}
