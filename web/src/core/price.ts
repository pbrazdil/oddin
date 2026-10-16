// Odds travel as decimal strings ("1.85") and are compared as exact
// integers in ten-thousandths, never as binary floats.

const PRICE = /^(\d{1,4})(?:\.(\d{1,4}))?$/;

/** Converts "1.85" to 18500. Throws on anything that is not a plain decimal. */
export function priceUnits(price: string): number {
  const m = PRICE.exec(price);
  if (!m) throw new Error(`invalid price ${JSON.stringify(price)}`);
  const whole = Number(m[1]);
  const frac = Number((m[2] ?? '').padEnd(4, '0'));
  return whole * 10_000 + frac;
}

/** Orders two prices: -1 if a < b, 0 if equal, 1 if a > b. */
export function comparePrices(a: string, b: string): -1 | 0 | 1 {
  const d = priceUnits(a) - priceUnits(b);
  return d < 0 ? -1 : d > 0 ? 1 : 0;
}
