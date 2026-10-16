// Builds the frontend into ../internal/webui/dist, which the Go binary embeds.
import * as esbuild from 'esbuild';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';

const out = path.resolve('../internal/webui/dist');
await rm(out, { recursive: true, force: true });
await mkdir(path.join(out, 'assets'), { recursive: true });

const common = { bundle: true, minify: true, legalComments: 'none', logLevel: 'warning' };

// The embeddable board (React). Content-hashed names, cached forever.
const app = await esbuild.build({
  ...common,
  entryPoints: { app: 'src/app/main.tsx' },
  outdir: path.join(out, 'assets'),
  entryNames: '[name]-[hash]',
  format: 'esm',
  target: ['es2022'],
  jsx: 'automatic',
  define: { 'process.env.NODE_ENV': '"production"' },
  metafile: true,
});
const outputs = Object.keys(app.metafile.outputs).map((p) => '/assets/' + path.basename(p));
const js = outputs.find((p) => p.endsWith('.js'));
const css = outputs.find((p) => p.endsWith('.css'));
const html = (await readFile('public/index.html', 'utf8')).replace('{{APP_JS}}', js).replace('{{APP_CSS}}', css);
await writeFile(path.join(out, 'index.html'), html);

// Stable-named scripts for partner pages.
await esbuild.build({ ...common, entryPoints: ['src/embed/embed.ts'], outfile: path.join(out, 'embed.js'), format: 'iife', target: ['es2018'] });
await esbuild.build({ ...common, entryPoints: ['src/embed/client.ts'], outfile: path.join(out, 'client.js'), format: 'esm', target: ['es2020'] });

await cp('public/demo', path.join(out, 'demo'), { recursive: true });
await writeFile(path.join(out, '.gitkeep'), '');
console.log(`built ${js}, ${css}, /embed.js, /client.js, /demo/`);
