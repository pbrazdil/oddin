// embed.js: the drop-in loader for partner pages written in plain HTML/JS.
//
//   <div data-oddsboard data-match="match-1" data-market="winner"></div>
//   <script src="https://ODDS-HOST/embed.js" async></script>
//
// or programmatically:
//
//   const board = OddsBoard.mount(element, { match: 'match-1', market: 'winner', theme: 'dark' });
//   element.addEventListener('oddsboard:select', (e) => addToBetSlip(e.detail));
//   board.destroy();
//
// The loader creates the iframe, sizes it to its content and re-dispatches
// the board's postMessage events as DOM events on the container
// (oddsboard:ready, oddsboard:resize, oddsboard:state, oddsboard:select).
// Messages are only accepted from the board's own origin and iframe.

interface MountOptions {
  match?: string;
  market?: string;
  theme?: 'light' | 'dark' | 'auto';
  title?: string;
  /** Superadmin token for the replay tools; for internal test pages only. */
  admin?: string;
}

interface Handle {
  iframe: HTMLIFrameElement;
  destroy(): void;
}

declare global {
  interface Window {
    OddsBoard?: { mount(el: HTMLElement, opts?: MountOptions): Handle; origin: string };
  }
}

(() => {
  const script = document.currentScript as HTMLScriptElement | null;
  const origin = script?.src ? new URL(script.src).origin : window.location.origin;

  function mount(el: HTMLElement, opts: MountOptions = {}): Handle {
    const match = opts.match || 'match-1';
    const market = opts.market || 'winner';
    const params = new URLSearchParams({ parentOrigin: window.location.origin });
    if (opts.theme) params.set('theme', opts.theme);
    const iframe = document.createElement('iframe');
    iframe.src =
      `${origin}/embed/${encodeURIComponent(match)}/${encodeURIComponent(market)}?${params}` +
      (opts.admin ? `#admin=${encodeURIComponent(opts.admin)}` : '');
    iframe.title = opts.title || 'Live odds';
    iframe.style.border = '0';
    iframe.style.width = '100%';
    iframe.style.display = 'block';
    iframe.style.height = '132px';
    iframe.style.colorScheme = 'normal';
    el.appendChild(iframe);

    const onMessage = (e: MessageEvent) => {
      if (e.origin !== origin || e.source !== iframe.contentWindow) return;
      const d = e.data as { source?: string; type?: string; height?: number } | null;
      if (!d || d.source !== 'oddsboard' || typeof d.type !== 'string') return;
      if (d.type === 'resize' && typeof d.height === 'number' && d.height > 0 && d.height < 10_000) {
        iframe.style.height = `${d.height}px`;
      }
      el.dispatchEvent(new CustomEvent(`oddsboard:${d.type}`, { detail: d, bubbles: true }));
    };
    window.addEventListener('message', onMessage);
    return {
      iframe,
      destroy() {
        window.removeEventListener('message', onMessage);
        iframe.remove();
      },
    };
  }

  window.OddsBoard = { mount, origin };

  const auto = () => {
    document.querySelectorAll<HTMLElement>('[data-oddsboard]:not([data-oddsboard-mounted])').forEach((el) => {
      el.setAttribute('data-oddsboard-mounted', '');
      const d = el.dataset;
      mount(el, { match: d.match, market: d.market, theme: d.theme as MountOptions['theme'], title: d.title });
    });
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', auto);
  else auto();
})();

export {};
