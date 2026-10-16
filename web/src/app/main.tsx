import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './App.tsx';
import { readConfig } from './config.ts';
import './styles.css';

let embedded = true;
try {
  embedded = window.self !== window.top;
} catch {
  // Accessing window.top can throw in some sandboxes: we are framed.
}

const cfg = readConfig(window.location, embedded);
// The admin token lives in the fragment, which changes without a reload.
window.addEventListener('hashchange', () => window.location.reload());
const html = document.documentElement;
html.classList.toggle('embedded', embedded);
if (cfg.theme !== 'auto') html.dataset.theme = cfg.theme;
if (cfg.adminToken) html.classList.add('admin-mode');

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App cfg={cfg} />
  </StrictMode>,
);
