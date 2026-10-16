import type { Blocker } from '../core/stream.ts';
import type { SeqRange } from '../core/types.ts';

/** What the customer sees when prices are not actionable. */
export function blockerText(b: Blocker | undefined): string {
  switch (b) {
    case undefined:
      return '';
    case 'connecting':
      return 'Connecting…';
    case 'reconnecting':
      return 'Connection interrupted, reconnecting…';
    case 'offline':
      return 'Offline: prices unavailable';
    case 'feed_delayed':
    case 'feed_stale':
      return 'Prices delayed: betting paused';
    case 'no_data':
      return 'Waiting for prices…';
    case 'incomplete':
    case 'sequence_gap':
    case 'conflict':
      return 'Updating prices…';
    case 'suspended':
      return 'Market suspended';
    case 'closed':
      return 'Market closed';
  }
}

/** Technical wording for superadmins. */
export function blockerDetail(b: Blocker): string {
  switch (b) {
    case 'sequence_gap':
      return 'missing feed messages';
    case 'conflict':
      return 'conflicting feed data';
    case 'incomplete':
      return 'incomplete data';
    case 'feed_stale':
      return 'feed stale (server)';
    case 'feed_delayed':
      return 'feed stale (heartbeat clock)';
    case 'no_data':
      return 'no data yet';
    default:
      return b;
  }
}

export function formatTime(iso: string, withDate = false): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (n: number, w = 2) => String(n).padStart(w, '0');
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
  if (!withDate) return time;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}

export function formatRanges(rs: SeqRange[]): string {
  return rs.map((r) => (r.from === r.to ? `${r.from}` : `${r.from}–${r.to}`)).join(', ');
}
