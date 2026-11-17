import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';
import postcss from 'postcss';
import tailwindcss from '@tailwindcss/postcss';

const input = path.resolve('pkg/server/assets/styles.css');
const output = path.resolve('pkg/server/assets/build/css/styles.css');
const source = await readFile(input, 'utf8');
const result = await postcss([tailwindcss({
  optimize: { minify: true },
  // The imported icon font URLs already target the emitted build directory.
  transformAssetUrls: false,
})]).process(source, {
  from: input,
  to: output,
});

await mkdir(path.dirname(output), { recursive: true });
// Keep the checked-in minified asset byte-for-byte reproducible.
await writeFile(output, result.css.replace(/\r?\n$/, ''));
