import type { MarketState, Published } from './types.ts';

export function state(over: Partial<MarketState> = {}): MarketState {
  return {
    matchId: 'match-1',
    marketId: 'winner',
    event: 'Falcons vs Wolves',
    market: 'Match winner',
    seq: 41,
    status: 'open',
    outcomes: [
      { id: 'falcons', name: 'Falcons', price: '1.85' },
      { id: 'wolves', name: 'Wolves', price: '2.05' },
    ],
    feed: 'live',
    consistent: true,
    gaps: [],
    conflicts: [],
    tradable: true,
    reasons: [],
    ...over,
  };
}

export function pub(version: number, over: Partial<MarketState> = {}, epoch = 'e1'): Published {
  return { epoch, version, staleAfterMs: 3000, state: state(over) };
}
