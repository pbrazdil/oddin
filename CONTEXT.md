# Live Odds Board

Build an embeddable live odds board for Falcons vs Wolves, showing the two outcomes of the match-winner market.

## Technical requirements

- React frontend and Go backend.
- The Go backend accepts feed events, maintains the current market state and delivers updates to the React frontend without a page refresh.
- Provide a way to submit or replay feed events.
- The frontend must work inside an iframe.

## Behaviour

- Display the current odds for both outcomes and visually indicate price movements.
- Prices and market availability change independently.
- Reflect market availability and connection health; unavailable or stale prices must not look actionable.
- After a feed interruption, the board must be able to return to a consistent current state.

## Feed

Feed events arrive approximately once per second. Example input:

```json
[
  {"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}},
  {"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}},
  {"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"},
  {"matchId":"match-1","marketId":"winner","seq":44,"status":"open"},
  {"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}
]
```
