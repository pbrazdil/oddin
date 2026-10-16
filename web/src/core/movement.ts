import { comparePrices } from './price.ts';
import type { Published } from './types.ts';

export type Direction = 'up' | 'down';

/**
 * Remembers the last price shown for each outcome and reports a direction
 * for a short while after it changes, so the board can flash the move.
 */
export class MovementTracker {
  private shown = new Map<string, string>();
  private moves = new Map<string, { dir: Direction; until: number }>();
  private epoch: string | null = null;
  private readonly flashMs: number;

  constructor(flashMs = 4000) {
    this.flashMs = flashMs;
  }

  observe(p: Published, now: number): void {
    if (p.epoch !== this.epoch) {
      // A new history: nothing on screen is a "previous" price for it.
      this.epoch = p.epoch;
      this.shown.clear();
      this.moves.clear();
    }
    for (const o of p.state.outcomes) {
      if (o.price === null) continue;
      const before = this.shown.get(o.id);
      if (before !== undefined && before !== o.price) {
        const c = comparePrices(o.price, before);
        if (c !== 0) this.moves.set(o.id, { dir: c > 0 ? 'up' : 'down', until: now + this.flashMs });
      }
      this.shown.set(o.id, o.price);
    }
  }

  /** The direction to show for an outcome right now, if any. */
  direction(id: string, now: number): Direction | null {
    const m = this.moves.get(id);
    return m && now < m.until ? m.dir : null;
  }

  /** When the next flash ends, for scheduling a re-render. */
  nextExpiry(now: number): number | null {
    let next: number | null = null;
    for (const m of this.moves.values()) {
      if (m.until > now && (next === null || m.until < next)) next = m.until;
    }
    return next;
  }
}
