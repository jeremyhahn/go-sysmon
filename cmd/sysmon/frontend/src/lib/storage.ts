// Guarded localStorage access for persisted UI preferences.
//
// Every entry point is wrapped, because localStorage throws rather than
// returning null in several real situations — Safari private browsing, a
// sandboxed iframe, cookies blocked entirely. A remembered page size is never
// worth failing a render over, so the failure mode here is always "fall back
// to the default", never an exception escaping to the caller.

/** readStored returns the raw stored string, or null when unavailable. */
export function readStored(key: string): string | null {
  try {
    return globalThis.localStorage?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

/** writeStored persists a string, silently doing nothing when it cannot. */
export function writeStored(key: string, value: string): void {
  try {
    globalThis.localStorage?.setItem(key, value);
  } catch {
    // Storage unavailable or full: the preference just does not persist.
  }
}

/**
 * readStoredJSON parses a stored value and validates its shape before handing
 * it back. Anything unparseable or unexpected yields null rather than a
 * half-typed object, since stored data is user-editable and survives upgrades
 * that may have changed its shape.
 */
export function readStoredJSON<T>(
  key: string,
  isValid: (value: unknown) => value is T
): T | null {
  const raw = readStored(key);
  if (raw == null) return null;
  try {
    const parsed: unknown = JSON.parse(raw);
    return isValid(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

/** writeStoredJSON serialises and persists a value, best-effort. */
export function writeStoredJSON(key: string, value: unknown): void {
  try {
    writeStored(key, JSON.stringify(value));
  } catch {
    // A value that cannot be serialised is simply not persisted.
  }
}
