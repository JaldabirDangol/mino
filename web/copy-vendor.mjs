// Copies third-party runtime files into the embedded static directory.
import { copyFileSync, mkdirSync } from "node:fs";

const out = "../internal/admin/static";
mkdirSync(`${out}/fonts`, { recursive: true });
const files = {
  "node_modules/htmx.org/dist/htmx.min.js": "htmx.min.js",
  "node_modules/alpinejs/dist/cdn.min.js": "alpine.min.js",
  "node_modules/@fontsource-variable/ibm-plex-sans/files/ibm-plex-sans-latin-wght-normal.woff2": "fonts/plex-sans-latin.woff2",
  "node_modules/@fontsource-variable/ibm-plex-sans/files/ibm-plex-sans-latin-ext-wght-normal.woff2": "fonts/plex-sans-latin-ext.woff2",
  "node_modules/@fontsource-variable/ibm-plex-sans/files/ibm-plex-sans-latin-wght-italic.woff2": "fonts/plex-sans-latin-italic.woff2",
  "node_modules/@fontsource-variable/schibsted-grotesk/files/schibsted-grotesk-latin-wght-normal.woff2": "fonts/schibsted-latin.woff2",
  "node_modules/@fontsource-variable/schibsted-grotesk/files/schibsted-grotesk-latin-ext-wght-normal.woff2": "fonts/schibsted-latin-ext.woff2",
  "node_modules/@fontsource/ibm-plex-mono/files/ibm-plex-mono-latin-400-normal.woff2": "fonts/plex-mono-400.woff2",
  "node_modules/@fontsource/ibm-plex-mono/files/ibm-plex-mono-latin-500-normal.woff2": "fonts/plex-mono-500.woff2",
};
for (const [src, dst] of Object.entries(files)) copyFileSync(src, `${out}/${dst}`);
console.log(`copied ${Object.keys(files).length} files`);
