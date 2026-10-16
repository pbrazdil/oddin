import type { Published } from './types.ts';

/**
 * Decides whether an incoming state replaces the one on screen. Within an
 * epoch only a strictly higher version is newer, so a late or repeated
 * message can never roll the board back. A different epoch means the server
 * restarted the market's history (reset or redeploy) and is adopted as is.
 */
export function isNewer(current: Published | null, incoming: Published): boolean {
  if (current === null) return true;
  if (incoming.epoch !== current.epoch) return true;
  return incoming.version > current.version;
}

/** Validates the shape of a parsed message before it is trusted. */
export function isPublished(v: unknown): v is Published {
  if (typeof v !== 'object' || v === null) return false;
  const p = v as Record<string, unknown>;
  const s = p.state as Record<string, unknown> | undefined;
  return (
    typeof p.epoch === 'string' &&
    typeof p.version === 'number' &&
    Number.isSafeInteger(p.version) &&
    typeof s === 'object' &&
    s !== null &&
    typeof s.tradable === 'boolean' &&
    typeof s.status === 'string' &&
    Array.isArray(s.outcomes) &&
    Array.isArray(s.reasons)
  );
}
