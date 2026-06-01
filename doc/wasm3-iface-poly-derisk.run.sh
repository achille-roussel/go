#!/bin/sh
# Validates the polymorphic interface derisk WAT and runs it on Node.js.
# Expected exit code: 2768632, encoding the four sub-test results:
#   (dispatchT1 << 16) | (dispatchT2 << 4) | (assertT1 << 3) | (assertT2 << 2)
# = (42 << 16) | (1007 << 4) | (1 << 3) | (0 << 2)
# = 2752512 | 16112 | 8 | 0 = 2768632
set -e
SRC="$(cd "$(dirname "$0")" && pwd)/wasm3-iface-poly-derisk.wat"
OUT=$(mktemp -t iface-derisk.wasm.XXXXXX)
trap "rm -f $OUT ${OUT}.js" EXIT

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
