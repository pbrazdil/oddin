// Wire types of the board API. The server is the only source of truth:
// every message carries the complete market state, never a delta, so the
// client has nothing to merge and cannot drift from the server.

export type Status = 'unknown' | 'open' | 'suspended' | 'closed';
export type FeedHealth = 'waiting' | 'live' | 'stale';
export type Reason =
  | 'no_data'
  | 'incomplete'
  | 'sequence_gap'
  | 'conflict'
  | 'feed_stale'
  | 'suspended'
  | 'closed';

export interface SeqRange {
  from: number;
  to: number;
}

export interface OutcomeState {
  id: string;
  name: string;
  /** Decimal odds as an exact decimal string, e.g. "1.85"; null until known. */
  price: string | null;
  seq?: number;
  prev?: string;
  move?: 'up' | 'down';
  changedAt?: string;
}

export interface MarketState {
  matchId: string;
  marketId: string;
  event: string;
  market: string;
  seq: number;
  status: Status;
  statusSeq?: number;
  outcomes: OutcomeState[];
  feed: FeedHealth;
  lastEventAt?: string;
  consistent: boolean;
  gaps: SeqRange[];
  conflicts: SeqRange[];
  /** The only flag that may make prices actionable. */
  tradable: boolean;
  reasons: Reason[];
}

export interface Published {
  /** Changes when the server resets the market; adopt the new state wholesale. */
  epoch: string;
  /** Strictly increasing within an epoch. */
  version: number;
  staleAfterMs: number;
  state: MarketState;
}

export interface Heartbeat {
  epoch: string;
  version: number;
  serverTime: string;
}
