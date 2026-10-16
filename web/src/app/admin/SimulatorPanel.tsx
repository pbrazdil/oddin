import { useState } from 'react';

import { useInterval } from '../hooks.ts';
import type { AdminApi, SimulatorSettings, SimulatorStats } from './api.ts';

/** Controls for the built-in provider simulator and its unreliable channel. */
export function SimulatorPanel({ api }: { api: AdminApi }) {
  const [settings, setSettings] = useState<SimulatorSettings | null>(null);
  const [stats, setStats] = useState<SimulatorStats | null>(null);
  const [error, setError] = useState('');

  useInterval(() => {
    api.simulator().then(
      (r) => {
        setSettings((s) => s ?? r.settings);
        setStats(r.stats);
        setSettings((s) => (s ? { ...s, running: r.settings.running, interruptedUntil: r.settings.interruptedUntil } : r.settings));
      },
      (e) => setError(String(e)),
    );
  }, 1000);

  if (!settings) return <p className="muted">{error || 'Loading…'}</p>;

  const apply = (next: SimulatorSettings) => {
    setSettings(next);
    api.configureSimulator(next).then((r) => setSettings(r.settings), (e) => setError(String(e)));
  };
  const interrupt = (seconds: number) =>
    api.interrupt(seconds).then((r) => setSettings(r.settings), (e) => setError(String(e)));
  const interrupted = settings.interruptedUntil && Date.parse(settings.interruptedUntil) > Date.now();

  const rate = (key: 'dropRate' | 'duplicateRate' | 'reorderRate', label: string) => (
    <label className="field">
      {label}: {Math.round(settings[key] * 100)}%
      <input type="range" min={0} max={0.5} step={0.05} value={settings[key]} onChange={(e) => apply({ ...settings, [key]: Number(e.target.value) })} />
    </label>
  );

  return (
    <div className="sim">
      <p className="muted">
        The simulator plays the odds provider: about one partial update per second, occasional suspensions, delivered over a channel that can lose, duplicate and
        reorder messages. Its snapshot endpoint is what the board uses to resync after a gap.
      </p>
      <div className="sim__row">
        <button type="button" className={settings.running ? 'btn--danger' : 'btn--primary'} onClick={() => apply({ ...settings, running: !settings.running })}>
          {settings.running ? 'Stop provider' : 'Start provider'}
        </button>
        <button type="button" onClick={() => void interrupt(5)}>
          Interrupt feed 5 s
        </button>
        <button type="button" onClick={() => void interrupt(15)}>
          Interrupt feed 15 s
        </button>
        {interrupted && (
          <button type="button" onClick={() => void interrupt(0)}>
            Resume now
          </button>
        )}
        {interrupted && <span className="badge badge--conflict">interrupted</span>}
      </div>
      <div className="sim__grid">
        <label className="field">
          Interval: {settings.intervalMs} ms
          <input type="range" min={200} max={3000} step={100} value={settings.intervalMs} onChange={(e) => apply({ ...settings, intervalMs: Number(e.target.value) })} />
        </label>
        {rate('dropRate', 'Loss')}
        {rate('duplicateRate', 'Duplicates')}
        {rate('reorderRate', 'Reordering')}
      </div>
      {stats && (
        <p className="muted">
          Provider seq {stats.seq} · generated {stats.generated} · delivered {stats.delivered} · dropped {stats.dropped} · duplicated {stats.duplicated} · reordered{' '}
          {stats.reordered}
        </p>
      )}
      {error && <p className="error">{error}</p>}
    </div>
  );
}
