import { useState } from 'react';

import { useInterval } from '../hooks.ts';
import type { Ack, AdminApi, ReplayStatus } from './api.ts';

const m = (body: string) => `{"matchId":"match-1","marketId":"winner",${body}}`;

/** Ready-made scenarios, written as literal JSON so they are sent byte for byte. */
const SCENARIOS: { name: string; events: string[] }[] = [
  {
    name: 'Brief example',
    events: [
      m('"seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}'),
      m('"seq":42,"odds":{"falcons":1.80}'),
      m('"seq":43,"status":"suspended"'),
      m('"seq":44,"status":"open"'),
      m('"seq":45,"odds":{"falcons":1.90,"wolves":1.95}'),
    ],
  },
  {
    name: 'Lost suspension (gap)',
    events: [m('"seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}'), m('"seq":45,"odds":{"falcons":1.90,"wolves":1.95}')],
  },
  {
    name: 'Out of order + late',
    events: [
      m('"seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}'),
      m('"seq":43,"status":"suspended"'),
      m('"seq":42,"odds":{"falcons":1.80}'),
      m('"seq":45,"odds":{"falcons":1.90,"wolves":1.95}'),
      m('"seq":44,"status":"open"'),
    ],
  },
  {
    name: 'Duplicate + conflict',
    events: [
      m('"seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}'),
      m('"seq":42,"odds":{"falcons":1.80}'),
      m('"seq":42,"odds":{"falcons":1.80}'),
      m('"seq":42,"odds":{"falcons":1.70}'),
      m('"seq":43,"status":"open","odds":{"falcons":1.75,"wolves":2.10}'),
    ],
  },
  {
    name: 'Invalid messages',
    events: [
      m('"seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}'),
      m('"seq":42,"odds":{"falcons":0.95}'),
      m('"seq":43,"odds":{"draw":3.10}'),
      m('"seq":44,"status":"paused"'),
      m('"seq":45,"status":"open","odds":{"falcons":1.90,"wolves":1.95}'),
    ],
  },
];

const asText = (events: string[]) => '[\n  ' + events.join(',\n  ') + '\n]';

interface Props {
  api: AdminApi;
  hasSimulator: boolean;
}

/** Submit or replay feed events by hand. */
export function FeedConsole({ api, hasSimulator }: Props) {
  const [text, setText] = useState(asText(SCENARIOS[0]!.events));
  const [intervalMs, setIntervalMs] = useState(1000);
  const [reset, setReset] = useState(true);
  const [stopSim, setStopSim] = useState(true);
  const [status, setStatus] = useState<ReplayStatus | null>(null);
  const [acks, setAcks] = useState<Ack[] | null>(null);
  const [error, setError] = useState('');

  useInterval(() => {
    api.replayStatus().then(setStatus, () => {});
  }, 1000);

  const run = async (fn: () => Promise<void>) => {
    setError('');
    try {
      await fn();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="console">
      <div className="console__scenarios">
        {SCENARIOS.map((s) => (
          <button key={s.name} type="button" className="chip" onClick={() => setText(asText(s.events))}>
            {s.name}
          </button>
        ))}
      </div>
      <textarea
        className="console__input"
        spellCheck={false}
        value={text}
        onChange={(e) => setText(e.target.value)}
        aria-label="Feed events as JSON"
        rows={8}
      />
      <div className="console__options">
        <label className="field field--inline">
          Interval
          <input type="number" min={0} max={60000} step={100} value={intervalMs} onChange={(e) => setIntervalMs(Number(e.target.value))} /> ms
        </label>
        <label className="check">
          <input type="checkbox" checked={reset} onChange={(e) => setReset(e.target.checked)} /> reset market first
        </label>
        {hasSimulator && (
          <label className="check">
            <input type="checkbox" checked={stopSim} onChange={(e) => setStopSim(e.target.checked)} /> stop simulator
          </label>
        )}
      </div>
      <div className="console__actions">
        <button
          type="button"
          className="btn--primary"
          disabled={status?.running}
          onClick={() =>
            run(async () => {
              setAcks(null);
              setStatus(await api.replay(text, intervalMs, reset, stopSim && hasSimulator));
            })
          }
        >
          Replay
        </button>
        <button type="button" disabled={!status?.running} onClick={() => run(async () => setStatus(await api.stopReplay()))}>
          Stop
        </button>
        <button
          type="button"
          title="Send the text exactly as typed to the feed endpoint, all at once"
          onClick={() =>
            run(async () => {
              setAcks((await api.submit(text)).acks);
            })
          }
        >
          Send now
        </button>
        <button
          type="button"
          title="Send the text as a full-state snapshot"
          onClick={() =>
            run(async () => {
              setAcks([(await api.snapshot(text)).ack]);
            })
          }
        >
          Send as snapshot
        </button>
        <button type="button" className="btn--danger" onClick={() => run(async () => void (await api.reset()))}>
          Reset market
        </button>
      </div>
      {error && <p className="error">{error}</p>}
      {status && (status.running || status.total > 0) && acks === null && (
        <p className="muted">
          Replay {status.running ? 'running' : status.error ? `ended: ${status.error}` : 'finished'}: {status.sent}/{status.total} sent
        </p>
      )}
      <AckList acks={acks ?? status?.acks ?? []} />
    </div>
  );
}

function AckList({ acks }: { acks: Ack[] }) {
  if (acks.length === 0) return null;
  return (
    <ol className="acks">
      {acks.map((a, i) => (
        <li key={i}>
          <span className="row__seq">{a.seq ? `#${a.seq}` : '—'}</span>
          <span className={`badge badge--${a.verdict}`}>{a.verdict}</span>
          <span className="muted">{a.reason}</span>
        </li>
      ))}
    </ol>
  );
}
