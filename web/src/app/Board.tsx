import { useEffect, useRef, useState } from 'react';

import type { Direction } from '../core/movement.ts';
import type { Blocker, Connection } from '../core/stream.ts';
import type { MarketState, OutcomeState } from '../core/types.ts';
import { blockerText, formatTime } from './format.ts';

export interface ReplayInfo {
  at: string;
  seq: number;
  recordId: number;
}

export interface BoardProps {
  state: MarketState | null;
  actionable: boolean;
  blockers: Blocker[];
  connection: Connection;
  moves: Record<string, Direction | null>;
  /** Set when showing a historical state; the board is then never actionable. */
  replay: ReplayInfo | null;
  onSelect?: (o: OutcomeState) => void;
}

export function Board({ state, actionable, blockers, connection, moves, replay, onSelect }: BoardProps) {
  const live = replay === null;
  const canAct = live && actionable;
  const notice = live ? (canAct ? '' : blockerText(blockers[0])) : 'Historical state, not live';
  const outcomes = state?.outcomes ?? [];

  return (
    <section
      className={`board${canAct ? '' : ' board--locked'}${live ? '' : ' board--replay'}`}
      aria-label={state ? `${state.event}, ${state.market}` : 'Live odds'}
    >
      {!live && (
        <div className="board__replay" role="status">
          <span className="board__replay-tag">Replay</span>
          {formatTime(replay.at, true)} · seq {replay.seq} · record #{replay.recordId}
        </div>
      )}
      <header className="board__head">
        <div className="board__title">
          <span className="board__event">{state?.event ?? 'Live odds'}</span>
          <span className="board__market">{state?.market ?? ''}</span>
        </div>
        <StatusPill state={state} stale={live && connection !== 'live'} />
        {live && <Health connection={connection} blockers={blockers} />}
      </header>
      <div className="board__outcomes">
        {outcomes.length === 0 && <div className="board__placeholder">—</div>}
        {outcomes.map((o) => (
          <OutcomeButton key={o.id} outcome={o} move={moves[o.id] ?? null} actionable={canAct} onSelect={onSelect} />
        ))}
      </div>
      <p className="board__notice" aria-live="polite">
        {notice}
      </p>
    </section>
  );
}

/** Market status. Without a live connection it is only the last known one, so it is shown muted. */
function StatusPill({ state, stale }: { state: MarketState | null; stale: boolean }) {
  const status = state?.status ?? 'unknown';
  const label = { unknown: '—', open: 'Open', suspended: 'Suspended', closed: 'Closed' }[status];
  return (
    <span className={`pill pill--${stale ? 'unknown' : status}`} title={stale ? 'Last known status' : undefined}>
      {label}
    </span>
  );
}

function Health({ connection, blockers }: { connection: Connection; blockers: Blocker[] }) {
  let kind = 'live';
  let label = 'Live';
  if (connection === 'connecting') [kind, label] = ['warn', 'Connecting'];
  else if (connection === 'reconnecting') [kind, label] = ['warn', 'Reconnecting'];
  else if (connection === 'offline') [kind, label] = ['down', 'Offline'];
  else if (blockers.includes('feed_stale') || blockers.includes('feed_delayed')) [kind, label] = ['warn', 'Delayed'];
  else if (blockers.some((b) => b === 'sequence_gap' || b === 'conflict' || b === 'incomplete')) [kind, label] = ['warn', 'Syncing'];
  return (
    <span className={`health health--${kind}`} title={`Connection: ${connection}`}>
      <span className="health__dot" aria-hidden="true" />
      {label}
    </span>
  );
}

function OutcomeButton({
  outcome,
  move,
  actionable,
  onSelect,
}: {
  outcome: OutcomeState;
  move: Direction | null;
  actionable: boolean;
  onSelect?: (o: OutcomeState) => void;
}) {
  const enabled = actionable && outcome.price !== null;
  // Re-check at click time: the state may have changed since render.
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;
  const [picked, setPicked] = useState(false);
  useEffect(() => {
    if (!picked) return;
    const t = setTimeout(() => setPicked(false), 1200);
    return () => clearTimeout(t);
  }, [picked]);

  const label =
    `${outcome.name}, odds ${outcome.price ?? 'unavailable'}` +
    (move ? (move === 'up' ? ', price up' : ', price down') : '') +
    (enabled ? '' : ', not available');

  return (
    <button
      type="button"
      className={`outcome${move ? ` outcome--${move}` : ''}${picked ? ' outcome--picked' : ''}`}
      disabled={!enabled}
      aria-label={label}
      onClick={() => {
        if (!enabledRef.current) return;
        setPicked(true);
        onSelect?.(outcome);
      }}
    >
      <span className="outcome__name">{outcome.name}</span>
      <span className="outcome__price">
        {move && (
          <span className="outcome__arrow" aria-hidden="true">
            {move === 'up' ? '▲' : '▼'}
          </span>
        )}
        {outcome.price ?? '—'}
        {!enabled && <LockIcon />}
      </span>
    </button>
  );
}

function LockIcon() {
  return (
    <svg className="outcome__lock" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true">
      <path d="M4.5 7V5a3.5 3.5 0 0 1 7 0v2h.5A1 1 0 0 1 13 8v6a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h.5Zm1.5 0h4V5a2 2 0 1 0-4 0v2Z" fill="currentColor" />
    </svg>
  );
}
