import { useEffect, useMemo, useRef, useState } from 'react';

import { comparePrices } from '../../core/price.ts';
import type { Direction } from '../../core/movement.ts';
import { blockerDetail, formatRanges, formatTime } from '../format.ts';
import { PriceChart } from './PriceChart.tsx';
import type { AdminApi, JournalRecord } from './api.ts';

export interface ReplayFrame {
  record: JournalRecord;
  moves: Record<string, Direction | null>;
}

type Filter = 'all' | 'changes' | 'problems';

interface Props {
  api: AdminApi;
  records: JournalRecord[];
  frame: ReplayFrame | null;
  onFrame: (f: ReplayFrame | null) => void;
}

const isProblem = (r: JournalRecord) =>
  r.verdict === 'rejected' ||
  r.verdict === 'conflict' ||
  r.verdict === 'late' ||
  r.kind === 'recovery' ||
  r.kind === 'snapshot' ||
  r.kind === 'health' ||
  (r.state !== undefined && (r.state.gaps.length > 0 || r.state.conflicts.length > 0));

const isChange = (r: JournalRecord) => (r.changes?.length ?? 0) > 0 || r.kind === 'reset' || r.kind === 'health';

/** Moves to flash when replaying a record: the prices it changed. */
export function movesOf(r: JournalRecord): Record<string, Direction | null> {
  const out: Record<string, Direction | null> = {};
  for (const ch of r.changes ?? []) {
    if (!ch.field.startsWith('odds.') || !ch.from) continue;
    const c = comparePrices(ch.to, ch.from);
    out[ch.field.slice(5)] = c > 0 ? 'up' : c < 0 ? 'down' : null;
  }
  return out;
}

/**
 * The superadmin track record: every message the board received, what it
 * decided and what customers saw afterwards. Selecting a record shows that
 * historical state on the board above; playback steps through them in real
 * (or accelerated) time.
 */
export function TrackRecord({ api, records, frame, onFrame }: Props) {
  const [filter, setFilter] = useState<Filter>('all');
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(4);
  const [lookup, setLookup] = useState('');
  const [lookupError, setLookupError] = useState('');
  const listRef = useRef<HTMLDivElement>(null);

  const withState = useMemo(() => records.filter((r) => r.state), [records]);
  const visible = useMemo(
    () => records.filter((r) => filter === 'all' || (filter === 'changes' ? isChange(r) : isProblem(r))),
    [records, filter],
  );
  const selectedId = frame?.record.id ?? null;
  const index = selectedId === null ? -1 : withState.findIndex((r) => r.id === selectedId);

  const select = (r: JournalRecord | undefined) => {
    if (!r) return;
    const withS = r.state ? r : [...withState].reverse().find((x) => x.id < r.id);
    if (!withS) return;
    onFrame({ record: { ...r, state: withS.state }, moves: r.state ? movesOf(r) : {} });
  };
  const step = (delta: number) => {
    const from = index === -1 ? withState.length - 1 : index;
    select(withState[Math.min(withState.length - 1, Math.max(0, from + delta))]);
  };

  // Playback at recorded pace, scaled by speed.
  useEffect(() => {
    if (!playing) return;
    if (index === -1 || index >= withState.length - 1) {
      setPlaying(false);
      return;
    }
    const cur = withState[index]!;
    const next = withState[index + 1]!;
    const gap = (Date.parse(next.at) - Date.parse(cur.at)) / speed;
    const t = setTimeout(() => select(next), Math.min(3000, Math.max(40, gap)));
    return () => clearTimeout(t);
  }, [playing, index, speed, withState]);

  // Follow the newest record while live.
  useEffect(() => {
    if (frame === null && listRef.current) listRef.current.scrollTop = listRef.current.scrollHeight;
  }, [records.length, frame === null, filter]);

  // Keep the selected row in view by scrolling the list only, so the board
  // above stays on screen.
  useEffect(() => {
    const list = listRef.current;
    const row = selectedId === null ? null : list?.querySelector<HTMLElement>(`[data-id="${selectedId}"]`);
    if (!list || !row) return;
    const top = row.offsetTop - list.offsetTop;
    if (top < list.scrollTop) list.scrollTop = top;
    else if (top + row.offsetHeight > list.scrollTop + list.clientHeight) list.scrollTop = top + row.offsetHeight - list.clientHeight;
  }, [selectedId]);

  const findAt = async () => {
    setLookupError('');
    const t = new Date(lookup);
    if (Number.isNaN(t.getTime())) {
      setLookupError('Enter a date and time');
      return;
    }
    try {
      const { record } = await api.stateAt(t);
      setPlaying(false);
      onFrame({ record, moves: movesOf(record) });
    } catch (e) {
      setLookupError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="track">
      <PriceChart records={records} selectedId={selectedId} onPick={(id) => select(records.find((r) => r.id === id))} />

      <div className="track__controls">
        <div className="btn-group" role="group" aria-label="Playback">
          <button type="button" onClick={() => select(withState[0])} title="First record">
            ⏮
          </button>
          <button type="button" onClick={() => step(-1)} title="Previous record">
            ◀
          </button>
          <button
            type="button"
            className="btn--primary"
            onClick={() => {
              if (index === -1 || index >= withState.length - 1) select(withState[0]);
              setPlaying((p) => !p);
            }}
            title={playing ? 'Pause' : 'Play from the selected record'}
          >
            {playing ? '❚❚' : '▶'}
          </button>
          <button type="button" onClick={() => step(1)} title="Next record">
            ▶|
          </button>
          <button type="button" onClick={() => select(withState[withState.length - 1])} title="Latest record">
            ⏭
          </button>
        </div>
        <label className="field field--inline">
          Speed
          <select value={speed} onChange={(e) => setSpeed(Number(e.target.value))}>
            {[1, 4, 16, 60].map((s) => (
              <option key={s} value={s}>
                {s}×
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          className={frame === null ? 'btn--live btn--active' : 'btn--live'}
          onClick={() => {
            setPlaying(false);
            onFrame(null);
          }}
        >
          ● Live
        </button>
        <form
          className="track__lookup"
          onSubmit={(e) => {
            e.preventDefault();
            void findAt();
          }}
        >
          <label className="field field--inline">
            State at
            <input type="datetime-local" step="0.001" value={lookup} onChange={(e) => setLookup(e.target.value)} />
          </label>
          <button type="submit">Show</button>
          {lookupError && <span className="error">{lookupError}</span>}
        </form>
      </div>

      <div className="track__filters" role="radiogroup" aria-label="Filter records">
        {(['all', 'changes', 'problems'] as Filter[]).map((f) => (
          <button key={f} type="button" role="radio" aria-checked={filter === f} className={filter === f ? 'chip chip--on' : 'chip'} onClick={() => setFilter(f)}>
            {{ all: 'All messages', changes: 'Changes', problems: 'Anomalies' }[f]}
          </button>
        ))}
        <span className="track__count">
          {visible.length} of {records.length} records
        </span>
      </div>

      <div className="track__body">
        <div className="timeline" ref={listRef} role="listbox" aria-label="Journal records">
          {visible.map((r) => (
            <div
              key={r.id}
              data-id={r.id}
              role="option"
              aria-selected={r.id === selectedId}
              className={`row${r.id === selectedId ? ' row--selected' : ''}`}
              onClick={() => {
                setPlaying(false);
                select(r);
              }}
            >
              <span className="row__time">{formatTime(r.at)}</span>
              <span className="row__seq">{r.seq ? `#${r.seq}` : ''}</span>
              <span className={`badge badge--${r.verdict ?? r.kind}`}>{r.verdict ?? r.kind}</span>
              <span className="row__summary">{r.summary}</span>
              <span className={`row__trade ${r.state?.tradable ? 'row__trade--yes' : 'row__trade--no'}`} title={r.state?.tradable ? 'tradable' : 'not tradable'}>
                {r.state ? (r.state.tradable ? '●' : '○') : ''}
              </span>
            </div>
          ))}
        </div>
        <RecordDetail record={frame?.record ?? null} />
      </div>
    </div>
  );
}

function RecordDetail({ record }: { record: JournalRecord | null }) {
  if (record === null) {
    return (
      <aside className="detail detail--empty">
        Select a record to see exactly what the board received and showed at that moment. Use “State at” to look up the moment a customer placed a bet.
      </aside>
    );
  }
  const s = record.state;
  return (
    <aside className="detail">
      <h3>
        Record #{record.id} <span className={`badge badge--${record.verdict ?? record.kind}`}>{record.verdict ?? record.kind}</span>
      </h3>
      <dl>
        <dt>Received</dt>
        <dd>{formatTime(record.at, true)}</dd>
        <dt>Source</dt>
        <dd>{record.source ?? '—'}</dd>
        <dt>Summary</dt>
        <dd>{record.summary}</dd>
        {record.reason && (
          <>
            <dt>Reason</dt>
            <dd>{record.reason}</dd>
          </>
        )}
        {s && (
          <>
            <dt>Board showed</dt>
            <dd>
              {s.status} · {s.outcomes.map((o) => `${o.name} ${o.price ?? '—'}`).join(' · ')}
            </dd>
            <dt>Tradable</dt>
            <dd className={s.tradable ? 'ok' : 'bad'}>{s.tradable ? 'yes' : `no (${s.reasons.map(blockerDetail).join(', ')})`}</dd>
            <dt>Feed seq</dt>
            <dd>
              {s.seq}
              {s.gaps.length > 0 && ` · missing ${formatRanges(s.gaps)}`}
              {s.conflicts.length > 0 && ` · conflicting ${formatRanges(s.conflicts)}`}
            </dd>
            <dt>Version</dt>
            <dd>
              {record.version} <span className="muted">epoch {record.epoch}</span>
            </dd>
          </>
        )}
      </dl>
      {record.payload && (
        <>
          <h4>Message exactly as received</h4>
          <pre className="payload">{record.payload}</pre>
        </>
      )}
    </aside>
  );
}
