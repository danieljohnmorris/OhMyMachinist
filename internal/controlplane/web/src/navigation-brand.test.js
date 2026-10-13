import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("navigation brand uses theme-aware Oh My Machinist logos", async () => {
  const source = await readFile(new URL("./main.jsx", import.meta.url), "utf8");
  const styles = await readFile(new URL("./styles.css", import.meta.url), "utf8");

  assert.match(source, /src="\/logo-light-transparent\.png"/);
  assert.match(source, /src="\/logo-dark-transparent\.png"/);
  assert.match(source, /className="brand-logo brand-logo-light"/);
  assert.match(source, /className="brand-logo brand-logo-dark"/);
  assert.match(styles, /\.brand-logo-light/);
  assert.match(styles, /\.dark \.brand-logo-light/);
  assert.match(styles, /\.dark \.brand-logo-dark/);
  assert.doesNotMatch(source, /function MachinistMark\(/);
});
