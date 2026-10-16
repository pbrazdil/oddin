// The partner page's own code. It uses only the public, documented surface:
// embed.js (OddsBoard.mount + DOM events) and client.js (MarketStream).
import { MarketStream, marketUrls } from '/client.js';

const admin = new URLSearchParams(location.hash.slice(1)).get('admin');
addEventListener('hashchange', () => location.reload());
document.getElementById('admin-note').hidden = !admin;

// 1. Drop-in script with a host-side bet slip.
const slot = document.getElementById('board-script');
window.OddsBoard.mount(slot, { match: 'match-1', market: 'winner', admin: admin || undefined });

const log = document.getElementById('log');
const slip = { pick: null, latest: null, actionable: false };
for (const type of ['ready', 'resize', 'state', 'select']) {
  slot.addEventListener(`oddsboard:${type}`, (e) => {
    const d = e.detail;
    const li = document.createElement('li');
    const extra =
      type === 'resize' ? `height=${d.height}` :
      type === 'state' ? `actionable=${d.actionable} status=${d.status} ${d.outcomes.map((o) => `${o.id}=${o.price}`).join(' ')}` :
      type === 'select' ? `${d.outcome} @ ${d.price} (v${d.version})` : '';
    li.textContent = `${new Date().toLocaleTimeString()} ${type} ${extra}`;
    log.prepend(li);
    while (log.children.length > 60) log.lastChild.remove();

    if (type === 'state') {
      slip.actionable = d.actionable;
      slip.latest = d.outcomes;
      renderSlip();
    }
    if (type === 'select') {
      slip.pick = d;
      renderSlip();
    }
  });
}

function renderSlip() {
  const { pick, latest, actionable } = slip;
  document.getElementById('slip-empty').hidden = !!pick;
  document.getElementById('slip-body').hidden = !pick;
  if (!pick) return;
  const now = latest?.find((o) => o.id === pick.outcome);
  const changed = now && now.price !== pick.price;
  document.getElementById('slip-line').textContent = `${pick.name} @ ${pick.price}`;
  const warn = document.getElementById('slip-warn');
  warn.hidden = actionable && !changed;
  warn.textContent = !actionable ? 'Market not available right now: bet slip locked.' : `Price changed to ${now.price}. Re-select to accept.`;
  document.getElementById('slip-place').disabled = !actionable || changed;
}

// 3. No iframe: plain DOM driven by the same stream client the board uses.
const stream = new MarketStream(marketUrls(location.origin, 'match-1', 'winner'));
stream.subscribe((view) => {
  const st = view.published?.state;
  document.getElementById('v-title').textContent = st ? `${st.event} · ${st.market}` : 'Live odds';
  document.getElementById('v-status').textContent = st ? st.status : view.connection;
  const prices = document.getElementById('v-prices');
  prices.replaceChildren(
    ...(st?.outcomes ?? []).map((o) => {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = `${o.name} ${o.price ?? '—'}`;
      b.disabled = !view.actionable || o.price === null;
      b.onclick = () => alert(`Selected ${o.name} @ ${o.price}`);
      return b;
    }),
  );
  document.getElementById('v-note').textContent = view.actionable ? '' : `Not available: ${view.blockers.join(', ')}`;
});
stream.start();
