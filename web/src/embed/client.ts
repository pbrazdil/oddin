// client.js: the board's stream client as a plain ES module, for pages that
// want live odds without the iframe or React.
//
//   import { MarketStream, marketUrls } from 'https://ODDS-HOST/client.js';
//   const stream = new MarketStream(marketUrls('https://ODDS-HOST', 'match-1', 'winner'));
//   stream.subscribe((view) => render(view));   // view.actionable gates betting
//   stream.start();
export { MarketStream, marketUrls } from '../core/stream.ts';
export type { StreamView, Blocker, Connection } from '../core/stream.ts';
export type { Published, MarketState, OutcomeState } from '../core/types.ts';
