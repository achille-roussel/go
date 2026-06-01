// Runs the fat-pointer-with-class prototype on V8 and prints results
// for each test. Expected output:
//   test-array         = 30n
//   test-struct        = 99n
//   test-chan-transit  = 123n

const fs = require("fs");
const wasmBytes = fs.readFileSync(__dirname + "/prototype.wasm");

(async () => {
  const { instance } = await WebAssembly.instantiate(wasmBytes, {});
  const results = {
    "test-array": instance.exports["test-array"](),
    "test-struct": instance.exports["test-struct"](),
    "test-chan-transit": instance.exports["test-chan-transit"](),
  };
  const expected = {
    "test-array": 30n,
    "test-struct": 99n,
    "test-chan-transit": 123n,
  };
  let ok = true;
  for (const [name, got] of Object.entries(results)) {
    const want = expected[name];
    const match = got === want ? "OK" : "FAIL";
    if (got !== want) ok = false;
    console.log(`${name.padEnd(20)} got=${got}\twant=${want}\t${match}`);
  }
  process.exit(ok ? 0 : 1);
})().catch((e) => {
  console.error("instantiate failed:", e);
  process.exit(1);
});
