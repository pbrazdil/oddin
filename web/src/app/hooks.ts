import { useEffect, useRef, useState } from 'react';

import { MovementTracker, type Direction } from '../core/movement.ts';
import { MarketStream, marketUrls, type StreamView } from '../core/stream.ts';
import type { Published } from '../core/types.ts';

const initialView: StreamView = {
  published: null,
  connection: 'connecting',
  actionable: false,
  blockers: ['connecting'],
  feedAgeMs: null,
  reconnects: 0,
  transport: 'sse',
};

/** Live view of one market over SSE. */
export function useMarketStream(matchId: string, marketId: string): StreamView {
  const [view, setView] = useState<StreamView>(initialView);
  useEffect(() => {
    const stream = new MarketStream(marketUrls(window.location.origin, matchId, marketId));
    const unsubscribe = stream.subscribe(setView);
    stream.start();
    const onVisible = () => stream.check();
    const onOnline = () => stream.reconnectNow();
    document.addEventListener('visibilitychange', onVisible);
    window.addEventListener('online', onOnline);
    window.addEventListener('offline', onVisible);
    return () => {
      unsubscribe();
      stream.stop();
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('online', onOnline);
      window.removeEventListener('offline', onVisible);
    };
  }, [matchId, marketId]);
  return view;
}

/** Price movement flashes for the outcomes of the published state. */
export function useMovement(published: Published | null): Record<string, Direction | null> {
  const tracker = useRef(new MovementTracker());
  const [, rerender] = useState(0);
  const now = Date.now();
  const seen = useRef<Published | null>(null);
  if (published !== null && published !== seen.current) {
    seen.current = published;
    tracker.current.observe(published, now);
  }
  const expiry = tracker.current.nextExpiry(now);
  useEffect(() => {
    if (expiry === null) return;
    const t = setTimeout(() => rerender((n) => n + 1), Math.max(0, expiry - Date.now()) + 10);
    return () => clearTimeout(t);
  }, [expiry]);
  const out: Record<string, Direction | null> = {};
  for (const o of published?.state.outcomes ?? []) out[o.id] = tracker.current.direction(o.id, now);
  return out;
}

/** Observes an element's size. */
export function useWidth<T extends HTMLElement>(): [React.RefObject<T | null>, number] {
  const ref = useRef<T | null>(null);
  const [width, setWidth] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => entry && setWidth(entry.contentRect.width));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, width];
}

/** Calls fn every ms while enabled. */
export function useInterval(fn: () => void, ms: number, enabled = true): void {
  const saved = useRef(fn);
  saved.current = fn;
  useEffect(() => {
    if (!enabled) return;
    saved.current();
    const t = setInterval(() => saved.current(), ms);
    return () => clearInterval(t);
  }, [ms, enabled]);
}
