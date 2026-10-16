# Live Odds Board

An embeddable live odds board for **Falcons vs Wolves**, match-winner market.
A Go backend (standard library only, Go 1.27) merges the feed, journals
every decision and streams full market states over Server-Sent Events. A
React frontend runs inside an iframe, and plain-HTML/JS partners can embed it
without any framework. The brief is in [CONTEXT.md](CONTEXT.md).

```bash
make build && ./bin/oddsboard        # or: go run ./cmd/oddsboard (frontend is prebuilt)
```

On start it prints its links (tokens persist in `data/`):

```
Board (iframe src)    http://localhost:8080/embed/match-1/winner
Partner demo page     http://localhost:8080/demo/
Superadmin + replay   http://localhost:8080/embed/match-1/winner#admin=<token>
Feed ingestion        POST http://localhost:8080/api/v1/feed/events   Authorization: Bearer <feed token>
```

A built-in provider simulator sends about one partial update per second, so
the board is live straight away. `-simulate=false` turns it off.

## Layout

| Path | What |
| --- | --- |
| `internal/market` | The merge engine: parsing, sequencing, consistency, tradability. Pure, no I/O. |
| `internal/journal` | Append-only, SHA-256 hash-chained audit journal (JSONL, fsync per record). |
| `internal/board` | Routes messages, journals then commits, versions and publishes states, feed health, snapshot recovery, restore from journal. |
| `internal/feed` | Provider simulator (lossy channel), paced replayer, HTTP snapshot source, CLI push. |
| `internal/server` | HTTP API, SSE stream, superadmin API, iframe/static serving. |
| `web/src/core` | Framework-free client: stream with watchdog and version guard, price math, postMessage bridge. |
| `web/src/app` | React board and superadmin tools. |
| `web/src/embed` | `embed.js` (drop-in loader) and `client.js` (stream client as an ES module). |

## Ground truth: how feed events are merged

Money depends on this, so the rules are strict and fail closed.

**Parsing** (`market/event.go`, `market/price.go`)
- Odds are exact fixed-point integers (1/10 000), parsed from the JSON
  literal. A value that would need rounding (`1.85001`), is ≤ 1.00 or is
  > 1000 is rejected, never rounded. Odds go to clients as strings (`"1.85"`).
- `seq` must be an integer literal in 1 … 2⁵³−1 (`41.0`, `"41"`, `4.1e1` are rejected).
- Member names are case-sensitive. Duplicate names (`"seq":1,"seq":2`),
  unknown members (`"Status"`), `null` values, unknown statuses and unknown
  outcomes reject the whole message. Nothing is ever partially applied.

**Merge rule:** every field (the status and each outcome's price) is a
last-writer-wins register ordered by `seq`. A message only overwrites a field
whose current value came from a lower seq. Absent fields stay as they are, so
prices and availability change independently. Arrival order does not matter:
any permutation of the same messages gives the same state.

**Consistency rule:** the state is *proven current* only when every field
is known and no unaccounted seq could have changed a field after the message
that last set it. An unaccounted seq is one that is missing, rejected, or
received twice with different content. Example: after 41 (open), with 42–44
lost and 45 price-only, the status from 41 might be stale (43 was a
suspension), so the board is **not tradable**. Once 44 arrives, or a
snapshot, every field is newer than the gap and trading resumes, even if 42
never comes.

| Verdict | Meaning |
| --- | --- |
| `applied` | new seq, merged, head advanced |
| `late` | older than the head and not seen before; merged field by field |
| `duplicate` | identical re-delivery; ignored (delivery is idempotent) |
| `conflict` | same seq, different content (or contradicts a snapshot); ignored and the seq is flagged |
| `rejected` | malformed or not for this market; ignored, the seq stays a gap |

**Snapshots** (`POST /api/v1/feed/snapshot`, or fetched for recovery) state
the complete market *as of a seq*. They vouch for all history up to that seq
and clear every gap and conflict at or below it.

**Tradable** = status `open` ∧ consistent ∧ feed live (a new seq within 3 s).
On the client it also requires a live connection. The client never merges
anything: it renders the server's full state.

### Tests

`go test ./internal/market` holds scenario tests for every rule above (the
brief's sequence, all 120 orderings of it, gaps, late, duplicate, conflicting
and invalid events, snapshots, staleness). It also runs model-based property
tests: thousands of random feeds with loss, duplication, reordering and
snapshots, checked after every message against an independent oracle. The
invariants checked are:

1. Each field equals the last-writer-wins oracle.
2. **Safety:** a consistent state equals the in-order truth at its head.
3. **Precision:** when nothing is unaccounted for, there is no false alarm.
4. Tradable implies consistent, open and complete.
5. A final snapshot always restores consistency.

Fuzzers cover the parsers and the merge (`make fuzz`). The suite was also
mutation-tested: deliberately broken rules are caught. In total there are
175 Go tests and 22 frontend tests.

```bash
make test     # go vet, go test -race, tsc, node --test
make fuzz
```

## Delivery and connection health

- `GET /api/v1/markets/{match}/{market}/stream` (SSE). Every `state` event is
  the full state with `epoch` and `version`. The first event after
  (re)connecting is the current state, so an interruption never needs
  client-side catch-up.
- `heartbeat` events every second carry the server's version and clock. The
  client:
  - treats more than 3.5 s of silence as a dead connection (prices lock, it reconnects);
  - re-fetches the state if a heartbeat announces a version it has not seen;
  - locks prices if the feed age measured on the server clock exceeds
    `staleAfterMs`, even before the server's own stale flag arrives.
- **Long-poll fallback.** Some networks buffer event streams until they end,
  for example corporate proxies and Cloudflare quick tunnels, which
  [do not support SSE](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/).
  If an open stream delivers nothing for 5 s, the client switches to
  long-polling `GET …/{market}?after=<epoch>.<version>&wait=2000`. That
  request returns as soon as the state changes, or after 2 s with the same
  state, which acts as the heartbeat. The full states, version guard and
  silence rule stay the same. A server that is simply down stays on SSE and
  reconnects.
- Within an epoch the client only accepts a higher version, so a late message
  never rolls the board back. A new epoch (market reset, in-memory restart)
  is adopted wholesale.
- Non-actionable prices stay visible but are hatched, greyed and locked,
  with the reason shown ("Market suspended", "Prices delayed: betting
  paused", "Reconnecting…", "Updating prices…"). Price moves flash green ▲ /
  red ▼ for 4 s.

## Feed interruption and recovery

- No new seq for 3 s → feed `stale`, not tradable (journaled as a health record).
- A market that stays inconsistent for 2 s fetches the provider's snapshot
  (the simulator's, or `-snapshot-url`), retrying every 5 s. That window
  gives late messages a chance to arrive first. Every attempt is journaled.
- Try it from the superadmin Simulator tab: interrupt the feed for 5 s and
  watch the board go Delayed, then Syncing, then resync and become tradable,
  all in the track record.

## Audit journal and superadmin replay

Each message is journaled (and `fsync`ed) **before** it changes the state or
reaches a client: the message exactly as received, the verdict, a
plain-language summary and the resulting state. The same goes for duplicates
and rejections. If the write fails, nothing is applied and the sender gets
503. Records are hash-chained, so editing, deleting or reordering lines is
detected on load. A torn final line from a crash is truncated, which is safe
because it was never committed.

On startup the board rebuilds itself by replaying the journal. Every recorded
verdict and state is re-derived and compared, and the board refuses to start
from a history it cannot reproduce. **Integrity → Verify journal** runs the
same check live.

The superadmin URL (`…/embed/match-1/winner#admin=<token>`, or
`/demo/#admin=<token>` to see it inside a partner page) adds these tools to the iframe:

- **Track record:** price chart with non-tradable periods shaded, every
  journal record with its verdict and summary, and filters (changes,
  anomalies). Selecting a record shows that historical state on the board in
  a clearly marked replay mode. Playback steps through history at 1–60×.
  **State at** answers "what did the board show when this customer placed
  their bet?" (also `GET /api/v1/admin/markets/{m}/{k}/at?t=…`).
- **Feed console:** ready-made scenarios (the brief's example, a lost
  suspension, out of order, conflict, invalid), sent byte-exact: replayed
  paced (optionally after a market reset), sent at once, or sent as a snapshot.
- **Simulator:** start/stop the provider, interrupt the feed, add loss,
  duplication and reordering.
- **Integrity:** hash-chain and replay verification.

The token lives in the URL fragment, which browsers never send to the
server or in `Referer`. It is used only as a Bearer header. This is a test
convenience; production would use real SSO and roles.

From the command line: `./bin/oddsboard replay -interval 1s testdata/example-feed.json`.

## Embedding

```html
<!-- 1. Drop-in: auto-height, DOM events on the container -->
<div data-oddsboard data-match="match-1" data-market="winner"></div>
<script src="https://ODDS-HOST/embed.js" async></script>

<!-- 2. Plain iframe -->
<iframe src="https://ODDS-HOST/embed/match-1/winner?theme=dark" width="100%" height="172" style="border:0"></iframe>
```

```js
// 3. No iframe: the same stream client the board uses
import { MarketStream, marketUrls } from 'https://ODDS-HOST/client.js';
const s = new MarketStream(marketUrls('https://ODDS-HOST', 'match-1', 'winner'));
s.subscribe((view) => render(view)); // view.actionable gates betting
s.start();
```

- The embed page sends no `X-Frame-Options`. Framing is controlled by CSP
  `frame-ancestors` (`-frame-ancestors "https://partner.example"`, default
  `*`). Scripts and connections are restricted to the board's own origin.
- No cookies or storage are used, so third-party storage blocking does not matter.
- Events posted to the host: `ready`, `resize`, `state` (actionable, status,
  prices) and `select` (on clicking an actionable price). `embed.js` checks
  the origin and source, then re-dispatches them as `oddsboard:*` DOM events.
  `parentOrigin` restricts `postMessage` targets. A selection is
  informational: bet placement must re-validate the price server-side.
- `?theme=light|dark` (default follows the system).

## API

| Method & path | Auth | |
| --- | --- | --- |
| `GET /api/v1/markets` | public | market list |
| `GET /api/v1/markets/{match}/{market}[?after=&wait=]` | public, CORS | current state; long-polls with `after`/`wait` (≤ 5 s) |
| `GET /api/v1/markets/{match}/{market}/stream` | public, CORS | SSE |
| `POST /api/v1/feed/events` | feed or admin | one event or an array; per-message acks |
| `POST /api/v1/feed/snapshot` | feed or admin | full-state snapshot |
| `GET /api/v1/admin/markets/{m}/{k}/history?after=&limit=` | admin | journal records |
| `GET /api/v1/admin/markets/{m}/{k}/at?t=RFC3339` | admin | state shown at time t |
| `POST /api/v1/admin/markets/{m}/{k}/reset` | admin | new epoch |
| `POST /api/v1/admin/markets/{m}/{k}/replay` | admin | `{events, intervalMs, reset, stopSimulator}` |
| `GET/DELETE /api/v1/admin/replay` | admin | replay status / stop |
| `GET/PUT /api/v1/admin/simulator`, `POST …/interrupt` | admin | simulator |
| `GET /api/v1/admin/verify` | admin | journal verification |

## Known limits and next steps

- **Single instance.** State is in memory, backed by a local journal.
  Scaling out needs a shared ordered log and fencing between instances.
- **Provider sequence resets** are treated as conflicts. The market fails
  closed until an admin reset. A real provider protocol would carry an epoch.
- **Unknown feed fields are rejected** (fail closed). A schema change needs a deploy.
- The journal grows without bound (about 2 MB per hour at 1 msg/s). It needs rotation and archival.
- Admin access is a capability URL meant for testing.
