import { test } from 'node:test';
import assert from 'node:assert/strict';

import { comparePrices, priceUnits } from './price.ts';
import { isNewer, isPublished } from './guard.ts';
import { MovementTracker } from './movement.ts';
import { HostBridge, safeTargetOrigin } from './host.ts';
import { pub, state } from './testutil.ts';

test('priceUnits is exact', () => {
  assert.equal(priceUnits('1.85'), 18500);
  assert.equal(priceUnits('1.8'), 18000);
  assert.equal(priceUnits('2'), 20000);
  assert.equal(priceUnits('1.0001'), 10001);
  assert.equal(priceUnits('1000.00'), 10_000_000);
  // 0.1 + 0.2 style float errors cannot happen.
  assert.equal(priceUnits('1.15') - priceUnits('1.05'), 1000);
  for (const bad of ['', '1.', '.5', '-1.2', '1e2', '1.23456', 'abc', ' 1.2']) {
    assert.throws(() => priceUnits(bad), bad);
  }
});

test('comparePrices', () => {
  assert.equal(comparePrices('1.90', '1.85'), 1);
  assert.equal(comparePrices('1.80', '1.85'), -1);
  assert.equal(comparePrices('1.8', '1.80'), 0);
  assert.equal(comparePrices('10.00', '9.99'), 1);
});

test('isNewer only moves forward within an epoch', () => {
  assert.equal(isNewer(null, pub(1)), true);
  assert.equal(isNewer(pub(5), pub(6)), true);
  assert.equal(isNewer(pub(5), pub(5)), false, 'repeat');
  assert.equal(isNewer(pub(5), pub(4)), false, 'stale message must not roll back');
  assert.equal(isNewer(pub(500), pub(1, {}, 'e2')), true, 'new epoch is adopted');
});

test('isPublished rejects malformed messages', () => {
  assert.equal(isPublished(pub(1)), true);
  for (const bad of [null, 1, 'x', {}, { epoch: 'e', version: '1', state: state() }, { epoch: 'e', version: 1 }, { epoch: 'e', version: 1.5, state: state() }]) {
    assert.equal(isPublished(bad), false, JSON.stringify(bad));
  }
});

test('MovementTracker flashes changes for a while', () => {
  const m = new MovementTracker(1000);
  m.observe(pub(1), 0);
  assert.equal(m.direction('falcons', 0), null, 'first sight is not a move');
  m.observe(pub(2, { outcomes: [{ id: 'falcons', name: 'Falcons', price: '1.80' }, { id: 'wolves', name: 'Wolves', price: '2.10' }] }), 100);
  assert.equal(m.direction('falcons', 200), 'down');
  assert.equal(m.direction('wolves', 200), 'up');
  assert.equal(m.nextExpiry(200), 1100);
  assert.equal(m.direction('falcons', 1100), null, 'expired');
  // Same price again keeps no move; null prices are ignored.
  m.observe(pub(3, { outcomes: [{ id: 'falcons', name: 'Falcons', price: '1.80' }, { id: 'wolves', name: 'Wolves', price: null }] }), 2000);
  assert.equal(m.direction('falcons', 2000), null);
  // A new epoch forgets previous prices.
  m.observe(pub(1, {}, 'e2'), 3000);
  assert.equal(m.direction('falcons', 3000), null);
});

test('HostBridge posts tagged messages and dedupes resizes', () => {
  const sent: Array<[unknown, string]> = [];
  const bridge = new HostBridge({ postMessage: (m, o) => sent.push([m, o]) }, 'https://host.example', 'match-1', 'winner');
  bridge.post('ready');
  bridge.resize(120.2);
  bridge.resize(120.9);
  bridge.resize(140);
  assert.equal(sent.length, 3);
  assert.deepEqual(sent[0], [{ source: 'oddsboard', type: 'ready', matchId: 'match-1', marketId: 'winner' }, 'https://host.example']);
  assert.deepEqual((sent[1]?.[0] as { height: number }).height, 121);
  new HostBridge(null, '*', 'm', 'w').post('ready'); // top-level page: no-op
});

test('safeTargetOrigin', () => {
  assert.equal(safeTargetOrigin('https://host.example'), 'https://host.example');
  assert.equal(safeTargetOrigin('http://localhost:3000'), 'http://localhost:3000');
  assert.equal(safeTargetOrigin(null), '*');
  assert.equal(safeTargetOrigin('javascript:alert(1)'), '*');
  assert.equal(safeTargetOrigin('https://host.example/path'), '*');
});
