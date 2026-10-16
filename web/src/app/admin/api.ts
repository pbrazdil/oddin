import type { MarketState } from '../../core/types.ts';

export type Verdict = 'applied' | 'late' | 'duplicate' | 'conflict' | 'rejected';

/** One entry of the server's audit journal. */
export interface JournalRecord {
  id: number;
  at: string;
  kind: 'event' | 'snapshot' | 'health' | 'reset' | 'recovery';
  source?: string;
  matchId?: string;
  marketId?: string;
  seq?: number;
  payload?: string;
  verdict?: Verdict;
  reason?: string;
  changes?: { field: string; from?: string; to: string }[];
  summary: string;
  epoch?: string;
  version?: number;
  state?: MarketState;
}

export interface Ack {
  seq?: number;
  verdict: Verdict;
  reason?: string;
  version?: number;
  recordId?: number;
}

export interface ReplayStatus {
  running: boolean;
  total: number;
  sent: number;
  intervalMs: number;
  startedAt?: string;
  acks: Ack[];
  error?: string;
}

export interface SimulatorSettings {
  running: boolean;
  intervalMs: number;
  dropRate: number;
  duplicateRate: number;
  reorderRate: number;
  interruptedUntil?: string;
}

export interface SimulatorStats {
  seq: number;
  generated: number;
  delivered: number;
  dropped: number;
  duplicated: number;
  reordered: number;
}

export interface VerifyReport {
  records: number;
  head: string;
  chainOk: boolean;
  chainError?: string;
  replayOk: boolean;
  replayError?: string;
}

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/** Superadmin API client. The token travels only in the Authorization header. */
export class AdminApi {
  private readonly token: string;
  private readonly market: string;

  constructor(token: string, matchId: string, marketId: string) {
    this.token = token;
    this.market = `/api/v1/admin/markets/${encodeURIComponent(matchId)}/${encodeURIComponent(marketId)}`;
  }

  private async req<T>(method: string, path: string, body?: unknown, raw = false): Promise<T> {
    const res = await fetch(path, {
      method,
      cache: 'no-store',
      headers: { Authorization: `Bearer ${this.token}`, ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
      body: body === undefined ? undefined : raw ? (body as string) : JSON.stringify(body),
    });
    const text = await res.text();
    let data: unknown = null;
    try {
      data = text ? JSON.parse(text) : null;
    } catch {
      /* not JSON */
    }
    if (!res.ok) {
      const msg = (data as { error?: string } | null)?.error ?? (text || res.statusText);
      throw new ApiError(res.status, msg);
    }
    return data as T;
  }

  session() {
    return this.req<{ ok: boolean; simulator: boolean; journalHead: string }>('GET', '/api/v1/admin/session');
  }
  history(after: number, limit = 2000) {
    return this.req<{ records: JournalRecord[]; journalHead: string }>('GET', `${this.market}/history?after=${after}&limit=${limit}`);
  }
  stateAt(t: Date) {
    return this.req<{ record: JournalRecord }>('GET', `${this.market}/at?t=${encodeURIComponent(t.toISOString())}`);
  }
  reset() {
    return this.req<{ epoch: string; version: number }>('POST', `${this.market}/reset`);
  }
  /** Replays events given as raw JSON text, so the server receives them byte for byte. */
  replay(eventsJSON: string, intervalMs: number, reset: boolean, stopSimulator: boolean) {
    const body = `{"intervalMs":${Math.trunc(intervalMs)},"reset":${reset},"stopSimulator":${stopSimulator},"events":${eventsJSON}}`;
    return this.req<ReplayStatus>('POST', `${this.market}/replay`, body, true);
  }
  replayStatus() {
    return this.req<ReplayStatus>('GET', '/api/v1/admin/replay');
  }
  stopReplay() {
    return this.req<ReplayStatus>('DELETE', '/api/v1/admin/replay');
  }
  /** Sends raw feed JSON exactly as typed, like the provider would. */
  submit(rawJSON: string) {
    return this.req<{ acks: Ack[] }>('POST', '/api/v1/feed/events', rawJSON, true);
  }
  snapshot(rawJSON: string) {
    return this.req<{ ack: Ack }>('POST', '/api/v1/feed/snapshot', rawJSON, true);
  }
  simulator() {
    return this.req<{ settings: SimulatorSettings; stats: SimulatorStats }>('GET', '/api/v1/admin/simulator');
  }
  configureSimulator(s: SimulatorSettings) {
    return this.req<{ settings: SimulatorSettings; stats: SimulatorStats }>('PUT', '/api/v1/admin/simulator', s);
  }
  interrupt(seconds: number) {
    return this.req<{ settings: SimulatorSettings; stats: SimulatorStats }>('POST', '/api/v1/admin/simulator/interrupt', { seconds });
  }
  verify() {
    return this.req<VerifyReport>('GET', '/api/v1/admin/verify');
  }
}
