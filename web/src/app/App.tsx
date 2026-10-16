import { useEffect, useMemo, useRef } from 'react';
import { useState } from 'react';

import { HostBridge } from '../core/host.ts';
import type { OutcomeState } from '../core/types.ts';
import { AdminPanel } from './admin/AdminPanel.tsx';
import type { ReplayFrame } from './admin/TrackRecord.tsx';
import { Board } from './Board.tsx';
import type { Config } from './config.ts';
import { useMarketStream, useMovement } from './hooks.ts';

export function App({ cfg }: { cfg: Config }) {
  const view = useMarketStream(cfg.matchId, cfg.marketId);
  const moves = useMovement(view.published);
  const [frame, setFrame] = useState<ReplayFrame | null>(null);
  const rootRef = useRef<HTMLElement>(null);

  const host = useMemo(
    () => new HostBridge(cfg.embedded ? window.parent : null, cfg.parentOrigin, cfg.matchId, cfg.marketId),
    [cfg],
  );

  useEffect(() => host.post('ready'), [host]);

  // Tell the host how tall we are so it can size the iframe.
  useEffect(() => {
    const el = rootRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => host.resize(el.getBoundingClientRect().height));
    ro.observe(el);
    return () => ro.disconnect();
  }, [host]);

  // Mirror availability to the host, e.g. to lock its bet slip too.
  const st = view.published?.state;
  const prices = st?.outcomes.map((o) => `${o.id}=${o.price}`).join(',');
  useEffect(() => {
    if (!st) return;
    host.post('state', {
      actionable: view.actionable,
      status: st.status,
      version: view.published?.version,
      epoch: view.published?.epoch,
      outcomes: st.outcomes.map((o) => ({ id: o.id, name: o.name, price: o.price })),
    });
  }, [host, view.actionable, st?.status, prices]);

  const onSelect = (o: OutcomeState) => {
    const p = view.published;
    if (!p || !view.actionable) return;
    // The host must still re-validate price and version with its betting
    // backend; the board is a display, not a source of accepted prices.
    host.post('select', { outcome: o.id, name: o.name, price: o.price, seq: o.seq, epoch: p.epoch, version: p.version });
  };

  return (
    <main ref={rootRef} className="ob">
      {frame ? (
        <Board
          state={frame.record.state ?? null}
          actionable={false}
          blockers={[]}
          connection="live"
          moves={frame.moves}
          replay={{ at: frame.record.at, seq: frame.record.state?.seq ?? 0, recordId: frame.record.id }}
        />
      ) : (
        <Board
          state={view.published?.state ?? null}
          actionable={view.actionable}
          blockers={view.blockers}
          connection={view.connection}
          moves={moves}
          replay={null}
          onSelect={onSelect}
        />
      )}
      {cfg.adminToken && (
        <AdminPanel token={cfg.adminToken} matchId={cfg.matchId} marketId={cfg.marketId} view={view} frame={frame} onFrame={setFrame} />
      )}
    </main>
  );
}
