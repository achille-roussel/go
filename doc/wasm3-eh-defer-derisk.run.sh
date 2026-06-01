#!/bin/sh
# Validates the EH derisk WAT and runs it on Node.js.
# Expected exit code: 4631 (0x1217), encoding the LIFO defer order
# the four sub-tests produced. See the WAT for the BCD-packed
# expectation breakdown.
set -e
SRC="$(cd "$(dirname "$0")" && pwd)/wasm3-eh-defer-derisk.wat"
OUT=$(mktemp -t eh-derisk.wasm.XXXXXX)
trap "rm -f $OUT" EXIT

wasm-tools parse "$SRC" -o "$OUT"
wasm-tools validate --features all "$OUT"

cat > "${OUT}.js" <<'JS'
const fs = require('fs');
const { WASI } = require('wasi');
const wasi = new WASI({ version: 'preview1', args: [], env: {} });
const bytes = fs.readFileSync(process.argv[2]);
WebAssembly.instantiate(bytes, {
  wasi_snapshot_preview1: wasi.wasiImport,
}).then(({ instance }) => {
  let code;
  try { code = wasi.start(instance); } catch (e) {}
  console.log(code);
  process.exitCode = 0;
});
JS
node "${OUT}.js" "$OUT"
rm -f "${OUT}.js"
