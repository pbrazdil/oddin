// Messages the board sends to the page that embeds it. Hosts listen with
// window.addEventListener('message', ...) and must check event.origin; the
// embed.js loader does that and re-dispatches them as DOM events.

export interface BoardMessage {
  source: 'oddsboard';
  type: 'ready' | 'resize' | 'state' | 'select';
  matchId: string;
  marketId: string;
  [k: string]: unknown;
}

export interface Poster {
  postMessage(message: unknown, targetOrigin: string): void;
}

/**
 * HostBridge posts board events to the parent window. Only public market
 * data is ever sent; targetOrigin restricts delivery to the host page when
 * the embedder told us its origin (embed.js does).
 */
export class HostBridge {
  private readonly parent: Poster | null;
  private readonly targetOrigin: string;
  private readonly matchId: string;
  private readonly marketId: string;
  private lastHeight = -1;

  constructor(parent: Poster | null, targetOrigin: string, matchId: string, marketId: string) {
    this.parent = parent;
    this.targetOrigin = targetOrigin;
    this.matchId = matchId;
    this.marketId = marketId;
  }

  post(type: BoardMessage['type'], data: Record<string, unknown> = {}): void {
    if (this.parent === null) return;
    const msg: BoardMessage = { ...data, source: 'oddsboard', type, matchId: this.matchId, marketId: this.marketId };
    this.parent.postMessage(msg, this.targetOrigin);
  }

  resize(height: number): void {
    const h = Math.ceil(height);
    if (h !== this.lastHeight) {
      this.lastHeight = h;
      this.post('resize', { height: h });
    }
  }
}

/** Accepts only an http(s) origin as postMessage target; otherwise "*". */
export function safeTargetOrigin(value: string | null): string {
  if (!value) return '*';
  try {
    const u = new URL(value);
    if ((u.protocol === 'https:' || u.protocol === 'http:') && u.origin === value) return value;
  } catch {
    /* fall through */
  }
  return '*';
}
