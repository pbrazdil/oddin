import { useEffect, useMemo, useRef, useState } from 'react';

import type { StreamView } from '../../core/stream.ts';
import { blockerDetail, formatRanges } from '../format.ts';
import { useInterval } from '../hooks.ts';
import { AdminApi, ApiError, type JournalRecord, type VerifyReport } from './api.ts';
import { FeedConsole } from './FeedConsole.tsx';
import { SimulatorPanel } from './SimulatorPanel.tsx';
import { TrackRecord, type ReplayFrame } from './TrackRecord.tsx';

type Tab = 'track' | 'feed' | 'sim' | 'integrity';
const MAX_RECORDS = 20_000;

interface Props {
  token: string;
  matchId: string;
  marketId: string;
  view: StreamView;
  frame: ReplayFrame | null;
  onFrame: (f: ReplayFrame | null) => void;
}

/** Superadmin tools, shown inside the iframe when opened with #admin=TOKEN. */
export function AdminPanel({ token, matchId, marketId, view, frame, onFrame }: Props) {
  const api = useMemo(() => new AdminApi(token, matchId, marketId), [token, matchId, marketId]);
  const [session, setSession] = useState<{ simulator: boolean } | null>(null);
  const [denied, setDenied] = useState('');
  const [tab, setTab] = useState<Tab>('track');
  const [records, setRecords] = useState<JournalRecord[]>([]);
  const cursor = useRef(0);

  useEffect(() => {
    api.session().then(setSession, (e) => setDenied(e instanceof ApiError && e.status === 401 ? 'This admin link is not valid.' : String(e)));
  }, [api]);

  // Tail the journal.
  useInterval(
    () => {
      api.history(cursor.current).then(
        ({ records: page }) => {
          if (page.length === 0) return;
          cursor.current = page[page.length - 1]!.id;
          setRecords((rs) => {
            const all = rs.concat(page);
            return all.length > MAX_RECORDS ? all.slice(all.length - MAX_RECORDS) : all;
          });
        },
        () => {},
      );
    },
    1000,
    session !== null,
  );

  if (denied) return <section className="admin admin--denied">{denied}</section>;
  if (!session) return <section className="admin">Checking admin access…</section>;

  const st = view.published?.state;
  return (
    <section className="admin" aria-label="Superadmin tools">
      <div className="admin__status">
        <strong>Superadmin</strong>
        <span>
          connection {view.connection} ({view.transport === 'sse' ? 'SSE' : 'long-poll'}) · feed {st?.feed ?? '—'} · seq {st?.seq ?? '—'} · v{view.published?.version ?? '—'}
          {view.feedAgeMs !== null && ` · feed age ${(view.feedAgeMs / 1000).toFixed(1)} s`}
          {view.reconnects > 0 && ` · reconnects ${view.reconnects}`}
        </span>
        {st && st.gaps.length > 0 && <span className="bad">missing {formatRanges(st.gaps)}</span>}
        {st && st.conflicts.length > 0 && <span className="bad">conflicting {formatRanges(st.conflicts)}</span>}
        <span className={view.actionable ? 'ok' : 'bad'}>{view.actionable ? 'tradable' : `locked: ${view.blockers.map(blockerDetail).join(', ')}`}</span>
      </div>
      <nav className="tabs" role="tablist">
        {(
          [
            ['track', 'Track record'],
            ['feed', 'Feed console'],
            ...(session.simulator ? [['sim', 'Simulator']] : []),
            ['integrity', 'Integrity'],
          ] as [Tab, string][]
        ).map(([id, label]) => (
          <button key={id} type="button" role="tab" aria-selected={tab === id} className={tab === id ? 'tab tab--on' : 'tab'} onClick={() => setTab(id)}>
            {label}
          </button>
        ))}
      </nav>
      <div className="admin__body">
        {tab === 'track' && <TrackRecord api={api} records={records} frame={frame} onFrame={onFrame} />}
        {tab === 'feed' && <FeedConsole api={api} hasSimulator={session.simulator} />}
        {tab === 'sim' && <SimulatorPanel api={api} />}
        {tab === 'integrity' && <Integrity api={api} />}
      </div>
    </section>
  );
}

function Integrity({ api }: { api: AdminApi }) {
  const [report, setReport] = useState<VerifyReport | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  return (
    <div className="integrity">
      <p className="muted">
        Every message and every state the board published is appended to a hash-chained journal before it takes effect. Verification re-reads the journal, checks the
        chain and replays every message through a fresh engine, confirming each recorded decision and state is reproducible.
      </p>
      <button
        type="button"
        className="btn--primary"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          setError('');
          api
            .verify()
            .then(setReport, (e) => setError(String(e)))
            .finally(() => setBusy(false));
        }}
      >
        {busy ? 'Verifying…' : 'Verify journal'}
      </button>
      {error && <p className="error">{error}</p>}
      {report && (
        <dl className="integrity__report">
          <dt>Records</dt>
          <dd>{report.records}</dd>
          <dt>Hash chain</dt>
          <dd className={report.chainOk ? 'ok' : 'bad'}>{report.chainOk ? 'intact' : report.chainError}</dd>
          <dt>Replay</dt>
          <dd className={report.replayOk ? 'ok' : 'bad'}>{report.replayOk ? 'every decision reproduced' : report.replayError}</dd>
          <dt>Head hash</dt>
          <dd>
            <code>{report.head}</code>
          </dd>
        </dl>
      )}
    </div>
  );
}
