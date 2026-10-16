import { safeTargetOrigin } from '../core/host.ts';

export interface Config {
  matchId: string;
  marketId: string;
  theme: 'light' | 'dark' | 'auto';
  /** Where postMessage events go; "*" unless the embedder told us. */
  parentOrigin: string;
  /** Superadmin token from the URL fragment (never sent to the server in the URL). */
  adminToken: string | null;
  embedded: boolean;
}

/** Reads /embed/{matchId}/{marketId}?theme=&parentOrigin=#admin=TOKEN */
export function readConfig(loc: Location, embedded: boolean): Config {
  const parts = loc.pathname.split('/').filter(Boolean);
  const q = new URLSearchParams(loc.search);
  const hash = new URLSearchParams(loc.hash.replace(/^#/, ''));
  const theme = q.get('theme');
  return {
    matchId: decodeURIComponent(parts[1] ?? 'match-1'),
    marketId: decodeURIComponent(parts[2] ?? 'winner'),
    theme: theme === 'light' || theme === 'dark' ? theme : 'auto',
    parentOrigin: safeTargetOrigin(q.get('parentOrigin')),
    adminToken: hash.get('admin'),
    embedded,
  };
}
