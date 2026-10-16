import { test } from 'node:test';
import assert from 'node:assert/strict';

import { MarketStream, type EventSourceLike, type PollResult, type StreamView } from './stream.ts';
import { pub } from './testutil.ts';
import type { Published } from './types.ts';

class FakeES implements EventSourceLike {
  readyState = 0;
  closed = false;
  private handlers = new Map<string, Array<(e: MessageEvent) => void>>();
  readonly url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    this.handlers.set(type, [...(this.handlers.get(type) ?? []), fn]);
  }
  close() {
    this.closed = true;
    this.readyState = 2;
  }
  emit(type: string, data?: unknown) {
    if (type !== 'error') this.readyState = 1;
    for (const fn of this.handlers.get(type) ?? []) fn({ data: typeof data === 'string' ? data : JSON.stringify(data) } as MessageEvent);
  }
}

function harness(fetchResult: () => unknown = () => Promise.reject(new Error('offline'))) {
  let now = 1_000_000;
  const timers: Array<{ at: number; fn: () => void; id: number }> = [];
  let nextId = 0;
  const sources: FakeES[] = [];
  const fetches: string[] = [];
  const polls: Array<{ url: string; resolve: (r: PollResult) => void; reject: (e: Error) => void; signal: AbortSignal }> = [];
  const s = new MarketStream({
    streamUrl: '/stream',
    stateUrl: '/state',
    silenceMs: 3500,
    createEventSource: (url) => {
      const es = new FakeES(url);
      sources.push(es);
      return es;
    },
    fetchState: (url) => {
      fetches.push(url);
      return Promise.resolve().then(fetchResult);
    },
    pollState: (url, signal) => new Promise<PollResult>((resolve, reject) => polls.push({ url, resolve, reject, signal })),
    now: () => now,
    setTimer: (fn, ms) => {
      const t = { at: now + ms, fn, id: nextId++ };
      timers.push(t);
      return t.id;
    },
    clearTimer: (id) => {
      const i = timers.findIndex((t) => t.id === id);
      if (i >= 0) timers.splice(i, 1);
    },
  });
  const views: StreamView[] = [];
  s.subscribe((v) => views.push(v));
  const advance = (ms: number) => {
    const end = now + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const t = timers[0];
      if (!t || t.at > end) break;
      timers.shift();
      now = t.at;
      t.fn();
    }
    now = end;
  };
  const last = () => views[views.length - 1]!;
  const es = () => sources[sources.length - 1]!;
  return { s, sources, es, fetches, polls, advance, last, flush: () => new Promise((r) => setImmediate(r)) };
}

test('shows the first state and becomes actionable', () => {
  const h = harness();
  h.s.start();
  assert.equal(h.last().connection, 'connecting');
  assert.equal(h.last().actionable, false);
  h.es().emit('state', pub(1));
  assert.equal(h.last().connection, 'live');
  assert.equal(h.last().actionable, true);
  assert.deepEqual(h.last().blockers, []);
});

test('never rolls back to an older version', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(5, { seq: 45 }));
  h.es().emit('state', pub(4, { seq: 44, status: 'suspended', tradable: false, reasons: ['suspended'] }));
  assert.equal(h.last().published?.version, 5);
  assert.equal(h.last().actionable, true);
});

test('server-side reasons block trading', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(2, { status: 'suspended', tradable: false, reasons: ['suspended'] }));
  assert.equal(h.last().actionable, false);
  assert.deepEqual(h.last().blockers, ['suspended']);
  h.es().emit('state', pub(3, { tradable: false, reasons: [] }));
  assert.equal(h.last().actionable, false, 'tradable=false always wins');
});

test('silence makes prices non-actionable and reconnects', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(1));
  h.advance(3000);
  assert.equal(h.last().actionable, true);
  h.advance(1000); // > 3.5s without a message
  assert.equal(h.last().connection, 'reconnecting');
  assert.equal(h.last().actionable, false);
  assert.equal(h.last().published?.version, 1, 'last state stays visible');
  assert.ok(h.sources[0]!.closed, 'dead connection closed');
  h.advance(10);
  assert.equal(h.sources.length, 2, 'new connection opened');

  // The new connection starts with the full current state.
  h.es().emit('state', pub(9, { seq: 49 }));
  assert.equal(h.last().connection, 'live');
  assert.equal(h.last().published?.state.seq, 49);
  assert.equal(h.last().actionable, true);
});

test('heartbeats keep the connection alive', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(1, { lastEventAt: '2026-10-16T18:00:00Z' }));
  for (let i = 0; i < 10; i++) {
    h.advance(1000);
    h.es().emit('heartbeat', { epoch: 'e1', version: 1, serverTime: '2026-10-16T18:00:01Z' });
  }
  assert.equal(h.sources.length, 1);
  assert.equal(h.last().actionable, true);
  assert.equal(h.last().feedAgeMs, 1000);
});

test('heartbeat clock detects a stale feed the state does not show yet', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(1, { lastEventAt: '2026-10-16T18:00:00Z' }));
  h.es().emit('heartbeat', { epoch: 'e1', version: 1, serverTime: '2026-10-16T18:00:05Z' });
  assert.equal(h.last().actionable, false);
  assert.deepEqual(h.last().blockers, ['feed_delayed']);
});

test('a heartbeat announcing an unseen version triggers a resync', async () => {
  const newer: Published = pub(7, { seq: 47 });
  const h = harness(() => newer);
  h.s.start();
  h.es().emit('state', pub(5));
  h.es().emit('heartbeat', { epoch: 'e1', version: 7, serverTime: '2026-10-16T18:00:01Z' });
  assert.deepEqual(h.fetches, ['/state']);
  await h.flush();
  assert.equal(h.last().published?.version, 7);
  // An up-to-date heartbeat does not.
  h.es().emit('heartbeat', { epoch: 'e1', version: 7, serverTime: '2026-10-16T18:00:02Z' });
  assert.equal(h.fetches.length, 1);
});

test('malformed state triggers a resync instead of being shown', async () => {
  const h = harness(() => pub(3));
  h.s.start();
  h.es().emit('state', pub(2));
  h.es().emit('state', '{not json');
  h.es().emit('state', { epoch: 'e1' });
  assert.equal(h.last().published?.version, 2);
  await h.flush();
  assert.equal(h.last().published?.version, 3);
});

test('closed connection reconnects with backoff, then reports offline', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(1));
  const first = h.es();
  first.readyState = 2;
  first.emit('error');
  assert.equal(h.last().connection, 'reconnecting');
  assert.equal(h.last().actionable, false);
  h.advance(600);
  assert.ok(h.sources.length >= 2, 'retried');
  // Keep failing for a while.
  for (let i = 0; i < 20; i++) {
    const es = h.es();
    es.readyState = 2;
    es.emit('error');
    h.advance(1000);
  }
  assert.equal(h.last().connection, 'offline');
  assert.deepEqual(h.last().blockers.slice(0, 1), ['offline']);
  // Events from a connection we already gave up on are ignored.
  const stale = h.es();
  stale.readyState = 2;
  stale.emit('error');
  stale.emit('state', pub(2));
  assert.equal(h.last().connection, 'offline');
  h.advance(60_000); // the backoff timer opens a fresh connection
  assert.notEqual(h.es(), stale);
  h.es().emit('state', pub(2));
  assert.equal(h.last().connection, 'live');
});

test('a new epoch is adopted even with a lower version', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(40));
  h.es().emit('state', pub(1, { seq: 0, tradable: false, reasons: ['no_data'], status: 'unknown' }, 'e2'));
  assert.equal(h.last().published?.epoch, 'e2');
  assert.equal(h.last().actionable, false);
});

test('stop closes everything', () => {
  const h = harness();
  h.s.start();
  h.s.stop();
  assert.ok(h.es().closed);
  h.advance(60_000);
  assert.equal(h.sources.length, 1, 'no reconnect after stop');
});

test('falls back to long-polling when an open stream stays silent', async () => {
  const h = harness();
  h.s.start();
  h.es().readyState = 1; // headers arrived, but a proxy holds the body back
  h.advance(4000);
  assert.equal(h.polls.length, 0, 'still within the grace period');
  h.advance(1500);
  assert.ok(h.sources[0]!.closed, 'stream closed');
  assert.equal(h.last().transport, 'poll');
  assert.equal(h.polls.length, 1);
  assert.equal(h.polls[0]!.url, '/state?wait=2000');

  h.polls[0]!.resolve({ body: pub(3, { lastEventAt: '2026-10-16T18:00:00Z' }), serverTime: '2026-10-16T18:00:01Z' });
  await h.flush();
  assert.equal(h.last().connection, 'live');
  assert.equal(h.last().actionable, true);
  assert.equal(h.last().feedAgeMs, 1000);

  // The next request tells the server what we hold.
  assert.equal(h.polls[1]!.url, '/state?wait=2000&after=e1.3');
  // An older state is ignored here too.
  h.polls[1]!.resolve({ body: pub(2, { seq: 1 }), serverTime: null });
  await h.flush();
  assert.equal(h.last().published?.version, 3);
  h.polls[2]!.resolve({ body: pub(4, { status: 'suspended', tradable: false, reasons: ['suspended'] }), serverTime: null });
  await h.flush();
  assert.equal(h.last().published?.version, 4);
  assert.equal(h.last().actionable, false);
  assert.equal(h.sources.length, 1, 'no further event streams');
});

test('long-polling locks prices when responses stop and recovers after errors', async () => {
  const h = harness();
  h.s.start();
  h.es().readyState = 1;
  h.advance(5600);
  h.polls[0]!.resolve({ body: pub(1), serverTime: null });
  await h.flush();
  assert.equal(h.last().actionable, true);

  // The pending request hangs past the silence limit.
  h.advance(4000);
  assert.equal(h.last().connection, 'reconnecting');
  assert.equal(h.last().actionable, false);
  assert.ok(h.polls[1]!.signal.aborted, 'hung request aborted');
  h.advance(10);
  const retry = h.polls[h.polls.length - 1]!;
  retry.reject(new Error('502'));
  await h.flush();
  assert.equal(h.last().actionable, false);
  h.advance(1000); // backoff
  await h.flush();
  h.polls[h.polls.length - 1]!.resolve({ body: pub(2), serverTime: null });
  await h.flush();
  assert.equal(h.last().connection, 'live');
  assert.equal(h.last().actionable, true);
  assert.equal(h.last().transport, 'poll');
});

test('stays on SSE when the stream delivers, or when the server is down', () => {
  const h = harness();
  h.s.start();
  h.es().emit('state', pub(1));
  for (let i = 0; i < 12; i++) {
    h.advance(1000);
    h.es().emit('heartbeat', { epoch: 'e1', version: 1, serverTime: '2026-10-16T18:00:01Z' });
  }
  assert.equal(h.polls.length, 0);
  assert.equal(h.last().transport, 'sse');

  const down = harness();
  down.s.start();
  down.es().emit('error'); // connection refused; the browser keeps retrying
  down.advance(12_000);
  assert.equal(down.polls.length, 0, 'an unreachable server is not a buffering proxy');
  assert.equal(down.last().transport, 'sse');
});

test('stop ends polling', async () => {
  const h = harness();
  h.s.start();
  h.es().readyState = 1;
  h.advance(5600);
  const pending = h.polls[0]!;
  h.s.stop();
  assert.ok(pending.signal.aborted);
  pending.resolve({ body: pub(1), serverTime: null });
  await h.flush();
  assert.equal(h.last().published, null);
  assert.equal(h.polls.length, 1);
});
