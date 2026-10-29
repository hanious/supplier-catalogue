import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { setTimeout } from "node:timers/promises";
import test from "node:test";

const origin = "http://localhost:55801";

async function request(path, body, key = randomUUID()) {
  const response = await fetch(origin + path, {
    method: body === undefined ? "GET" : "POST",
    headers: body === undefined ? {} : { "Content-Type": "application/json", "Idempotency-Key": key },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30000),
  });
  const value = await response.json();
  assert.ok(response.ok, `${path}: ${response.status} ${JSON.stringify(value)}`);
  return value;
}

async function waitFor(description, read, ready) {
  const deadline = Date.now() + 15 * 60 * 1000;
  let report = Date.now() + 30000;
  for (;;) {
    const value = await read();
    if (ready(value)) return value;
    assert.ok(Date.now() < deadline, `${description} timed out: ${JSON.stringify(value)}`);
    if (Date.now() >= report) {
      console.log(`${description}: ${JSON.stringify(value.job || value)}`);
      report = Date.now() + 30000;
    }
    await setTimeout(500);
  }
}

async function finished(job, status = "completed") {
  const result = await waitFor(job.scenario, () => request("/api/status"), value => {
    assert.equal(value.job?.id, job.id);
    if (value.job.status === "running") return false;
    assert.equal(value.job.status, status, JSON.stringify(value.job));
    return true;
  });
  return result.job;
}

async function reset() {
  const key = randomUUID();
  const body = { confirm: "reset synthetic demo" };
  const job = await request("/api/demo/reset", body, key);
  assert.equal((await request("/api/demo/reset", body, key)).id, job.id);
  await finished(job);
  await waitFor("retired data cleanup", () => request("/api/status"), value => !value.cleanup_pending);
  assert.equal((await request("/api/products")).total_items, 100);
}

test("container HTTP contracts cover full import, reset and recovery scenarios", { timeout: 25 * 60 * 1000 }, async t => {
  const page = await fetch(origin);
  assert.equal(page.status, 200);
  assert.match(await page.text(), /Supplier catalogue/i);
  assert.ok(page.headers.get("content-security-policy"));

  const status = await request("/api/status");
  // A completed full import also exercises restoration after the client exits.
  let full = status.job;
  if (full?.scenario !== "full" || !["running", "completed"].includes(full.status)) {
    full = await request("/api/updates/start", { scenario: "full" });
  }
  full = await finished(full);
  assert.equal(full.processed, 10000);
  assert.equal(full.total, 10000);
  const products = await request("/api/products?sort=price&order=desc&limit=25");
  assert.equal(products.total_items, 10000);
  assert.equal(products.total_pages, 400);
  for (let i = 1; i < products.items.length; i++) {
    assert.ok(products.items[i - 1].price >= products.items[i].price);
  }
  assert.equal((await request("/api/products?q=P10000")).total_items, 1);
  const changes = await request(`/api/changes?update_id=${full.id}&page=100&limit=100`);
  assert.equal(changes.total_items, 10000);
  assert.equal(changes.items.length, 100);
  const initial = await fetch(origin + "/api/products");
  assert.equal(initial.status, 200);
  await initial.arrayBuffer();
  assert.ok(initial.headers.get("etag"));
  const cached = await fetch(origin + "/api/products", { headers: { "If-None-Match": initial.headers.get("etag") } });
  assert.equal(cached.status, 304);
  t.diagnostic("10,000-record import, global table queries and catalogue validator passed");

  await reset();
  const normal = await finished(await request("/api/updates/start", { scenario: "normal" }));
  assert.deepEqual(normal.counts, { added: 10, changed: 20, withdrawn: 5, ignored: 0, issues: 0 });
  assert.equal((await request("/api/products?status=withdrawn")).total_items, 5);
  await reset();
  const duplicate = await finished(await request("/api/updates/start", { scenario: "duplicates" }));
  assert.equal(duplicate.processed, 70);
  assert.equal(duplicate.counts.ignored, 35);
  assert.equal((await request(`/api/changes?update_id=${duplicate.id}`)).total_items, 35);
  const ordered = await finished(await request("/api/updates/start", { scenario: "out_of_order" }));
  assert.deepEqual(ordered.counts, { added: 0, changed: 1, withdrawn: 1, ignored: 2, issues: 0 });
  const invalid = await finished(await request("/api/updates/start", { scenario: "invalid" }), "finished_with_issues");
  assert.equal(invalid.counts.issues, 2);
  await finished(await request("/api/updates/start", { scenario: "correction" }));
  assert.equal((await request("/api/issues?status=open")).total_items, 1);
  assert.equal((await request("/api/issues?status=resolved")).total_items, 1);

  const outage = await finished(await request("/api/updates/start", { scenario: "outage" }), "interrupted");
  assert.equal(outage.processed, 30);
  const pending = await request("/api/demo/publish", { scenario: "normal" });
  await request("/api/demo/restore", { update_id: outage.id });
  assert.equal((await request("/api/status")).job.status, "interrupted");
  await request(`/api/updates/${outage.id}/resume`, {});
  const recovered = await finished(outage);
  assert.equal(recovered.processed, 100);
  const next = await request("/api/updates/start", { scenario: "normal" });
  assert.equal(next.id, pending.id);
  await finished(next);
  await finished(await request("/api/updates/start", { scenario: "slow" }));
  t.diagnostic("Reset, duplicates, chronology, corrections, supplier outage and retained-update recovery passed");
});
