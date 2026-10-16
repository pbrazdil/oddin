import { useMemo } from 'react';

import { priceUnits } from '../../core/price.ts';
import { useWidth } from '../hooks.ts';
import type { JournalRecord } from './api.ts';

const HEIGHT = 150;
const PAD = { top: 10, right: 44, bottom: 20, left: 8 };
const COLORS = ['var(--series-1)', 'var(--series-2)'];

interface Props {
  records: JournalRecord[];
  selectedId: number | null;
  onPick: (id: number) => void;
}

interface Point {
  t: number;
  id: number;
  tradable: boolean;
  reset: boolean;
  prices: (number | null)[];
}

/**
 * The track record of both prices over time. Shaded bands mark periods in
 * which prices were not tradable (suspended, stale, out of sync).
 */
export function PriceChart({ records, selectedId, onPick }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();

  const { points, names } = useMemo(() => {
    const points: Point[] = [];
    let names: string[] = [];
    for (const r of records) {
      if (!r.state) continue;
      names = r.state.outcomes.map((o) => o.name);
      points.push({
        t: Date.parse(r.at),
        id: r.id,
        tradable: r.state.tradable,
        reset: r.kind === 'reset',
        prices: r.state.outcomes.map((o) => (o.price === null ? null : priceUnits(o.price) / 10_000)),
      });
    }
    return { points, names };
  }, [records]);

  if (points.length < 2) {
    return (
      <div ref={ref} className="chart chart--empty">
        Not enough history yet
      </div>
    );
  }

  const t0 = points[0]!.t;
  const t1 = Math.max(points[points.length - 1]!.t, t0 + 1);
  let lo = Infinity;
  let hi = -Infinity;
  for (const p of points) for (const v of p.prices) if (v !== null) [lo, hi] = [Math.min(lo, v), Math.max(hi, v)];
  if (!Number.isFinite(lo)) [lo, hi] = [1, 2];
  const span = Math.max(hi - lo, 0.1);
  lo -= span * 0.1;
  hi += span * 0.1;

  const w = Math.max(width, 200);
  const innerW = w - PAD.left - PAD.right;
  const innerH = HEIGHT - PAD.top - PAD.bottom;
  const x = (t: number) => PAD.left + ((t - t0) / (t1 - t0)) * innerW;
  const y = (v: number) => PAD.top + (1 - (v - lo) / (hi - lo)) * innerH;

  // Step lines: a price holds until the next record changes it.
  const paths = (points[0]?.prices ?? []).map((_, i) => {
    let d = '';
    let prevY: number | null = null;
    for (const p of points) {
      const v = p.prices[i];
      if (v === null || v === undefined) {
        prevY = null;
        continue;
      }
      const px = x(p.t);
      const py = y(v);
      d += prevY === null ? `M${px.toFixed(1)},${py.toFixed(1)}` : `H${px.toFixed(1)}V${py.toFixed(1)}`;
      prevY = py;
    }
    if (prevY !== null) d += `H${x(t1).toFixed(1)}`;
    return d;
  });

  const bands: { x0: number; x1: number }[] = [];
  for (let i = 0; i < points.length; i++) {
    const p = points[i]!;
    if (p.tradable) continue;
    const end = points[i + 1]?.t ?? t1;
    const last = bands[bands.length - 1];
    if (last && Math.abs(last.x1 - x(p.t)) < 0.5) last.x1 = x(end);
    else bands.push({ x0: x(p.t), x1: x(end) });
  }

  const ticks = [lo + (hi - lo) * 0.15, (lo + hi) / 2, hi - (hi - lo) * 0.15];
  const selected = points.find((p) => p.id === selectedId);

  const pick = (clientX: number, rect: DOMRect) => {
    const t = t0 + ((clientX - rect.left - PAD.left) / innerW) * (t1 - t0);
    let best = points[0]!;
    for (const p of points) if (p.t <= t) best = p;
    onPick(best.id);
  };

  return (
    <div ref={ref} className="chart">
      <svg
        width={w}
        height={HEIGHT}
        role="img"
        aria-label="Price history; shaded areas were not tradable"
        onClick={(e) => pick(e.clientX, e.currentTarget.getBoundingClientRect())}
      >
        {bands.map((b, i) => (
          <rect key={i} className="chart__locked" x={b.x0} y={PAD.top} width={Math.max(1, b.x1 - b.x0)} height={innerH} />
        ))}
        {points
          .filter((p) => p.reset)
          .map((p) => (
            <line key={p.id} className="chart__reset" x1={x(p.t)} x2={x(p.t)} y1={PAD.top} y2={PAD.top + innerH} />
          ))}
        {ticks.map((v) => (
          <g key={v}>
            <line className="chart__grid" x1={PAD.left} x2={PAD.left + innerW} y1={y(v)} y2={y(v)} />
            <text className="chart__label" x={PAD.left + innerW + 6} y={y(v) + 4}>
              {v.toFixed(2)}
            </text>
          </g>
        ))}
        {paths.map((d, i) => (
          <path key={i} d={d} fill="none" stroke={COLORS[i % COLORS.length]} strokeWidth={2} />
        ))}
        {selected && <line className="chart__cursor" x1={x(selected.t)} x2={x(selected.t)} y1={PAD.top - 4} y2={PAD.top + innerH + 4} />}
        <text className="chart__label" x={PAD.left} y={HEIGHT - 4}>
          {new Date(t0).toLocaleTimeString()}
        </text>
        <text className="chart__label" x={PAD.left + innerW} y={HEIGHT - 4} textAnchor="end">
          {new Date(t1).toLocaleTimeString()}
        </text>
      </svg>
      <div className="chart__legend">
        {names.map((n, i) => (
          <span key={n}>
            <i style={{ background: COLORS[i % COLORS.length] }} />
            {n}
          </span>
        ))}
        <span>
          <i className="chart__legend-locked" />
          not tradable
        </span>
      </div>
    </div>
  );
}
