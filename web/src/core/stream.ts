import { isNewer, isPublished } from './guard.ts';
import type { Heartbeat, Published, Reason } from './types.ts';

export type Connection = 'connecting' | 'live' | 'reconnecting' | 'offline';

/** How updates arrive: an event stream, or long-polling where streams are buffered. */
export type Transport = 'sse' | 'poll';

/** Why prices are not actionable, in display priority. */
export type Blocker = 'connecting' | 'reconnecting' | 'offline' | 'feed_delayed' | Reason;

export interface StreamView {
  published: Published | null;
  connection: Connection;
  /** True only when the server says tradable and our view is provably current. */
  actionable: boolean;
  blockers: Blocker[];
  /** Age of the last feed message on the server's clock, from the last heartbeat. */
  feedAgeMs: number | null;
  reconnects: number;
  transport: Transport;
}

/** The subset of EventSource the stream uses, so tests can fake it. */
export interface EventSourceLike {
  readonly readyState: number;
  addEventListener(type: string, listener: (e: MessageEvent) => void): void;
  close(): void;
}

export interface PollResult {
  body: unknown;
  /** The server's X-Server-Time header, if present. */
  serverTime: string | null;
}

export interface StreamOptions {
  streamUrl: string;
  stateUrl: string;
  /** Treat the connection as dead after this long without any message. */
  silenceMs?: number;
  /** Switch to long-polling if an open event stream delivers nothing for this long. */
  sseGraceMs?: number;
  /** How long the server may hold a long-poll request. Must stay below silenceMs. */
  pollWaitMs?: number;
  createEventSource?: (url: string) => EventSourceLike;
  fetchState?: (url: string) => Promise<unknown>;
  pollState?: (url: string, signal: AbortSignal) => Promise<PollResult>;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
}

const OPEN = 1;
const CLOSED = 2;
/** After this long without a message the board says "offline", not "reconnecting". */
const OFFLINE_AFTER_MS = 10_000;

/**
 * MarketStream keeps one market's state current over Server-Sent Events.
 * It has no framework dependency and is what both the React board and plain
 * JavaScript integrations use.
 *
 * Safety rules:
 *  - a state is only shown if it is newer than the one on screen (isNewer);
 *  - the connection counts as live only while messages keep arriving
 *    (heartbeats come every second), otherwise prices stop being actionable
 *    and the stream reconnects;
 *  - a reconnect always starts with the server's full current state, so no
 *    local merging or catch-up logic is needed after an interruption;
 *  - a heartbeat announcing a version we have not seen triggers a resync.
 *
 * Some networks buffer event streams until they end, so an open stream may
 * never deliver anything (Cloudflare quick tunnels, some corporate proxies).
 * The client then falls back to long-polling the state endpoint: the same
 * full states, the same version guard, the same silence rule, at the cost
 * of one request per change.
 */
export class MarketStream {
  private readonly opt: Required<StreamOptions>;
  private es: EventSourceLike | null = null;
  private published: Published | null = null;
  private connection: Connection = 'connecting';
  private lastMessageAt = 0;
  private startedAt = 0;
  private feedAgeMs: number | null = null;
  private attempts = 0;
  private reconnects = 0;
  private watchdog: unknown = null;
  private retry: unknown = null;
  private stopped = true;
  private listeners = new Set<(v: StreamView) => void>();
  private transport: Transport = 'sse';
  private sseDelivered = false;
  private sseErrored = false;
  private connectedAt = 0;
  private pollGen = 0;
  private pollAbort: AbortController | null = null;

  constructor(opt: StreamOptions) {
    this.opt = {
      silenceMs: 3500,
      sseGraceMs: 5000,
      pollWaitMs: 2000,
      createEventSource: (url) => new EventSource(url),
      pollState: defaultPoll,
      fetchState: (url) => fetch(url, { cache: 'no-store' }).then((r) => (r.ok ? r.json() : Promise.reject(new Error(r.statusText)))),
      now: () => Date.now(),
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
      ...opt,
    };
  }

  subscribe(fn: (v: StreamView) => void): () => void {
    this.listeners.add(fn);
    fn(this.view());
    return () => this.listeners.delete(fn);
  }

  start(): void {
    if (!this.stopped) return;
    this.stopped = false;
    this.startedAt = this.opt.now();
    this.connect();
    this.scheduleWatchdog();
  }

  stop(): void {
    this.stopped = true;
    this.es?.close();
    this.es = null;
    this.stopPolling();
    if (this.watchdog !== null) this.opt.clearTimer(this.watchdog);
    if (this.retry !== null) this.opt.clearTimer(this.retry);
    this.watchdog = this.retry = null;
  }

  /** Re-check liveness now, e.g. when the page becomes visible again. */
  check(): void {
    if (this.stopped) return;
    const silent = this.opt.now() - this.lastMessageAt > this.opt.silenceMs;
    if (this.connection === 'live' && silent) {
      this.setConnection('reconnecting');
      this.reconnect(0);
    }
  }

  /** Reconnect immediately, e.g. when the browser comes back online. */
  reconnectNow(): void {
    if (!this.stopped) this.reconnect(0);
  }

  view(): StreamView {
    const p = this.published;
    const blockers: Blocker[] = [];
    if (this.connection !== 'live') blockers.push(this.connection);
    if (p !== null && this.feedAgeMs !== null && this.feedAgeMs > p.staleAfterMs && !p.state.reasons.includes('feed_stale')) {
      // The server has not flagged it yet, but its own clock says the feed
      // is older than allowed. Err on the side of not trading.
      blockers.push('feed_delayed');
    }
    if (p === null) {
      if (this.connection === 'live') blockers.push('no_data');
    } else {
      blockers.push(...p.state.reasons);
      if (!p.state.tradable && p.state.reasons.length === 0) blockers.push('incomplete');
    }
    return {
      published: p,
      connection: this.connection,
      actionable: p !== null && p.state.tradable && blockers.length === 0,
      blockers,
      feedAgeMs: this.feedAgeMs,
      reconnects: this.reconnects,
      transport: this.transport,
    };
  }

  private emit(): void {
    const v = this.view();
    for (const fn of this.listeners) fn(v);
  }

  private setConnection(c: Connection): void {
    if (this.connection !== c) {
      this.connection = c;
      this.emit();
    }
  }

  private connect(): void {
    if (this.transport === 'poll') return this.startPolling();
    this.es?.close();
    const es = this.opt.createEventSource(this.opt.streamUrl);
    this.es = es;
    this.connectedAt = this.opt.now();
    this.sseErrored = false;
    const mine = (fn: (e: MessageEvent) => void) => (e: MessageEvent) => {
      if (this.es === es) fn(e);
    };
    es.addEventListener('state', mine((e) => this.onState(e.data)));
    es.addEventListener('heartbeat', mine((e) => this.onHeartbeat(e.data)));
    es.addEventListener('error', mine(() => this.onError(es)));
  }

  private alive(): void {
    this.lastMessageAt = this.opt.now();
    this.attempts = 0;
    this.connection = 'live';
  }

  private onState(data: string): void {
    let p: unknown;
    try {
      p = JSON.parse(data);
    } catch {
      return this.resync();
    }
    if (!isPublished(p)) return this.resync();
    this.sseDelivered = true;
    this.alive();
    this.accept(p);
    this.emit();
  }

  private accept(p: Published): void {
    if (isNewer(this.published, p)) {
      this.published = p;
      this.feedAgeMs = null; // until the next heartbeat measures it
    }
  }

  private onHeartbeat(data: string): void {
    let hb: Heartbeat;
    try {
      hb = JSON.parse(data) as Heartbeat;
    } catch {
      return;
    }
    this.sseDelivered = true;
    this.alive();
    const p = this.published;
    if (p !== null && p.state.lastEventAt) {
      this.feedAgeMs = Date.parse(hb.serverTime) - Date.parse(p.state.lastEventAt);
    }
    if (p === null || hb.epoch !== p.epoch || hb.version > p.version) this.resync();
    this.emit();
  }

  /** Fetch the current state over plain HTTP, e.g. after a missed update. */
  private resync(): void {
    this.opt
      .fetchState(this.opt.stateUrl)
      .then((p) => {
        if (!this.stopped && isPublished(p)) {
          this.accept(p);
          this.emit();
        }
      })
      .catch(() => {
        /* the stream itself will recover or time out */
      });
  }

  private onError(es: EventSourceLike): void {
    if (this.stopped) return;
    this.sseErrored = true;
    // While CONNECTING the browser retries by itself; once CLOSED (e.g. an
    // HTTP error) it has given up and we take over with backoff.
    if (this.connection !== 'offline') this.setConnection('reconnecting');
    if (es.readyState === CLOSED) this.reconnect(this.backoff());
  }

  private backoff(): number {
    const base = Math.min(30_000, 500 * 2 ** this.attempts);
    return base / 2 + Math.random() * (base / 2);
  }

  private reconnect(delay: number): void {
    if (this.retry !== null) this.opt.clearTimer(this.retry);
    this.es?.close();
    this.es = null;
    this.stopPolling();
    this.attempts++;
    this.reconnects++;
    this.retry = this.opt.setTimer(() => {
      this.retry = null;
      if (!this.stopped) this.connect();
    }, delay);
  }

  private scheduleWatchdog(): void {
    this.watchdog = this.opt.setTimer(() => {
      this.watchdog = null;
      if (this.stopped) return;
      if (this.streamLooksBuffered()) this.switchToPolling();
      if (this.connection === 'live') {
        this.check();
      } else {
        if (this.es === null && this.retry === null && this.pollAbort === null) this.reconnect(this.backoff());
        const quietFor = this.opt.now() - (this.lastMessageAt || this.startedAt);
        if (quietFor > OFFLINE_AFTER_MS) this.setConnection('offline');
      }
      this.scheduleWatchdog();
    }, 500);
  }

  /**
   * An event stream that is open (or connecting without errors) but has
   * never delivered a message is being held back by something in between.
   * A server that is merely down produces errors instead, and stays on SSE.
   */
  private streamLooksBuffered(): boolean {
    if (this.transport !== 'sse' || this.sseDelivered || this.es === null) return false;
    const waited = this.opt.now() - this.connectedAt;
    if (this.es.readyState === OPEN) return waited > this.opt.sseGraceMs;
    return !this.sseErrored && waited > 2 * this.opt.sseGraceMs;
  }

  private switchToPolling(): void {
    this.es?.close();
    this.es = null;
    this.transport = 'poll';
    this.startPolling();
    this.emit();
  }

  private stopPolling(): void {
    this.pollGen++;
    this.pollAbort?.abort();
    this.pollAbort = null;
  }

  private startPolling(): void {
    this.stopPolling();
    const gen = this.pollGen;
    const current = () => gen === this.pollGen && !this.stopped;
    const loop = async () => {
      while (current()) {
        const p = this.published;
        const q = `wait=${this.opt.pollWaitMs}` + (p ? `&after=${encodeURIComponent(`${p.epoch}.${p.version}`)}` : '');
        const abort = new AbortController();
        this.pollAbort = abort;
        try {
          const { body, serverTime } = await this.opt.pollState(`${this.opt.stateUrl}?${q}`, abort.signal);
          if (!current()) return;
          if (!isPublished(body)) throw new Error('malformed state');
          this.alive();
          this.accept(body);
          const cur = this.published;
          if (serverTime && cur?.state.lastEventAt) this.feedAgeMs = Date.parse(serverTime) - Date.parse(cur.state.lastEventAt);
          this.emit();
        } catch {
          if (!current()) return;
          if (this.connection !== 'offline') this.setConnection('reconnecting');
          const delay = this.backoff();
          this.attempts++;
          await new Promise<void>((resolve) => this.opt.setTimer(resolve, delay));
        }
      }
    };
    void loop();
  }
}

async function defaultPoll(url: string, signal: AbortSignal): Promise<PollResult> {
  // Abort requests the server or network holds for too long.
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10_000);
  signal.addEventListener('abort', () => controller.abort(), { once: true });
  try {
    const res = await fetch(url, { cache: 'no-store', signal: controller.signal });
    if (!res.ok) throw new Error(res.statusText);
    return { body: await res.json(), serverTime: res.headers.get('X-Server-Time') };
  } finally {
    clearTimeout(timeout);
  }
}

/** Builds the API URLs for a market on a given server origin. */
export function marketUrls(origin: string, matchId: string, marketId: string) {
  const path = `/api/v1/markets/${encodeURIComponent(matchId)}/${encodeURIComponent(marketId)}`;
  return { stateUrl: origin + path, streamUrl: origin + path + '/stream' };
}
