"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const api = window.CATALOGUE_API || "";
  const state = {
    job: null,
    inspectedUpdate: null,
    cleanupPending: false,
    baselineReady: false,
    generation: null,
    epoch: null,
    view: "catalogue",
    cursor: "",
    pages: [],
    next: "",
    timer: null,
    busy: false,
    pending: null,
    cache: new Map(),
    viewSequence: 0,
  };
  const labels = {
    running: "Updating",
    interrupted: "Needs attention · resume available",
    completed: "Completed",
    finished_with_issues: "Finished with issues",
    abandoned: "Abandoned by demo reset",
    unavailable: "Cannot resume · supplier update unavailable",
  };
  const expectations = {
    normal:
      "After Reset demo: add 10, change 20, withdraw 5. All 35 records saved.",
    duplicates:
      "After Reset demo: add 10, change 20, withdraw 5, ignore 35 duplicates. A repeated batch and a whole-update replay must not inflate counts.",
    out_of_order:
      "After Reset demo: change 1, withdraw 1, ignore 2 older records. The withdrawn product stays withdrawn.",
    outage:
      "Save 300 records, retry the supplier three times, then interrupt. Restore supplier, then Resume update; previously saved records stay saved.",
    invalid:
      "Save 2 valid changes, show 2 issues, and finish with issues. Existing valid prices stay saved.",
    slow: "Import 10,000 records. Close and reopen this page; processing continues and saved progress reloads.",
    full: "Import all 10,000 records, with durable progress and a terminal supplier marker. Duration depends on storage throughput.",
    correction:
      "Send newer valid details for two records. A prior product issue resolves; an unidentified record still needs a supplier correction with its source identity.",
  };
  const money = (cents) =>
    new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: "USD",
    }).format(cents / 100);
  const text = (value) => document.createTextNode(String(value ?? ""));
  function cell(value, className) {
    const td = document.createElement("td");
    if (className) td.className = className;
    td.append(value instanceof Node ? value : text(value));
    return td;
  }
  function pill(value, attention = false) {
    const span = document.createElement("span");
    span.className = "pill" + (attention ? " attention" : "");
    span.textContent = value;
    return span;
  }
  function productName(p) {
    const span = document.createElement("span");
    span.append(text(p.name || "Unidentified product"));
    const id = document.createElement("span");
    id.className = "product-id";
    id.textContent = p.id || "No usable product ID";
    span.append(id);
    return span;
  }
  function showError(message) {
    $("error").textContent = message;
    $("error").hidden = !message;
  }
  async function request(path, options = {}) {
    const response = await fetch(api + path, {
      ...options,
      signal: AbortSignal.timeout(15000),
      headers: { "Content-Type": "application/json", ...options.headers },
    });
    if (!response.ok) {
      const body = await response.json().catch(() => ({}));
      throw new Error(body.error || `Request failed (${response.status}).`);
    }
    return response.json();
  }
  function clearPoll() {
    clearTimeout(state.timer);
    state.timer = null;
  }
  function buttons() {
    const active = state.job?.status === "running";
    $("start").disabled =
      state.busy ||
      !state.baselineReady ||
      active ||
      state.job?.status === "interrupted";
    $("resume").hidden = state.job?.status !== "interrupted";
    $("resume").disabled = state.busy;
    $("reset").disabled = state.busy || active || state.cleanupPending;
    $("restore").disabled = state.busy || !state.job;
    $("publish").disabled = state.busy;
    $("replay").disabled = state.busy || !state.job || active;
  }
  async function status() {
    clearPoll();
    try {
      const data = await request("/api/status");
      const previous = state.job;
      const changed =
        state.generation !== data.generation ||
        state.epoch !== data.epoch ||
        previous?.status !== data.job?.status ||
        previous?.id !== data.job?.id;
      state.job = data.job;
      state.generation = data.generation;
      state.epoch = data.epoch;
      state.cleanupPending = data.cleanup_pending;
      state.baselineReady = data.baseline_ready;
      const j = state.job;
      $("update-heading").textContent = j
        ? `${j.id.slice(0, 13)} · ${labels[j.status] || j.status}`
        : "Ready for an update";
      $("message").textContent = j
        ? j.message ||
          `${j.processed.toLocaleString()} / ${j.total.toLocaleString()} records processed · saved changes are visible now.`
        : data.baseline_ready
          ? "10,000-product baseline loaded. Choose a scenario, then start an update."
          : "Use Reset demo to load the 10,000-product baseline before starting a scenario.";
      $("progress").max = j?.total || 10000;
      $("progress").value = j?.processed || 0;
      $("progress-label").textContent = j
        ? `${j.processed.toLocaleString()} of ${j.total.toLocaleString()} records processed`
        : "No update in progress";
      for (const name of ["added", "changed", "withdrawn", "ignored"])
        $(name).textContent = j?.counts[name]?.toLocaleString() || "0";
      $("issue-count").textContent = j?.counts.issues?.toLocaleString() || "0";
      if (j)
        $("observed").textContent =
          `Observed: ${j.counts.added} added, ${j.counts.changed} changed, ${j.counts.withdrawn} withdrawn, ${j.counts.ignored} ignored, ${j.counts.issues} issues. ${labels[j.status] || j.status}.`;
      if (previous?.status !== j?.status || previous?.id !== j?.id)
        $("announcement").textContent = j
          ? labels[j.status]
          : "Saved catalogue loaded.";
      $("connection").textContent = data.cleanup_pending
        ? "Baseline saved · clearing previous demo records. Refresh status before resetting again."
        : "Latest saved catalogue · status checked just now";
      showError("");
      buttons();
      if (changed) await loadView();
      if (j?.status === "running") state.timer = setTimeout(status, 2000);
    } catch (error) {
      $("connection").textContent =
        "Connection lost · showing the last saved view";
      showError(
        `${error.message} The backend may still be processing. Refresh saved status to reconnect.`,
      );
      buttons();
    }
  }
  async function action(path, body, idempotent = true) {
    if (state.busy) return;
    state.busy = true;
    buttons();
    showError("");
    // Keep the same key after a failed response, including uncertain network errors.
    const signature = path + JSON.stringify(body);
    if (state.pending?.signature !== signature)
      state.pending = { signature, key: crypto.randomUUID() };
    try {
      const data = await request(path, {
        method: "POST",
        body: JSON.stringify(body),
        headers: idempotent ? { "Idempotency-Key": state.pending.key } : {},
      });
      state.pending = null;
      if (path.endsWith("publish")) {
        $("published").textContent =
          `Published ${data.id.slice(0, 13)}. The current update keeps its original remaining pages. Start this update separately when the current one finishes.`;
        $("published").hidden = false;
      }
      if (path.endsWith("reset")) {
        state.cursor = "";
        state.pages = [];
        state.cache.clear();
        $("published").hidden = true;
      }
      await status();
    } catch (error) {
      showError(error.message);
    } finally {
      state.busy = false;
      buttons();
    }
  }
  function headers(titles) {
    const tr = document.createElement("tr");
    for (const title of titles) {
      const th = document.createElement("th");
      th.scope = "col";
      th.textContent = title;
      tr.append(th);
    }
    $("table-head").replaceChildren(tr);
  }
  function difference(result) {
    const before = result.before,
      after = result.after;
    if (!before)
      return `${after.name} · ${money(after.price)} · ${after.active ? "Active" : "Withdrawn"}`;
    const fields = [];
    if (before.name !== after.name)
      fields.push(`Name: ${before.name} → ${after.name}`);
    if (before.price !== after.price)
      fields.push(`Price: ${money(before.price)} → ${money(after.price)}`);
    if (before.active !== after.active)
      fields.push(
        `Status: ${before.active ? "Active" : "Withdrawn"} → ${after.active ? "Active" : "Withdrawn"}`,
      );
    return fields.join("; ");
  }
  async function loadView() {
    const sequence = ++state.viewSequence;
    const query = new URLSearchParams({ cursor: state.cursor, limit: "50" });
    let endpoint = state.view === "catalogue" ? "products" : state.view;
    if (state.view === "catalogue") {
      query.set("q", $("search").value);
      query.set("status", $("filter-status").value);
    }
    if (state.view === "changes") {
      if (!state.job && !state.inspectedUpdate) {
        state.next = "";
        render([]);
        return;
      }
      query.set("update_id", state.inspectedUpdate || state.job.id);
    }
    const path = `/api/${endpoint}?${query}`;
    const cached = state.cache.get(path);
    try {
      const response = await fetch(api + path, {
        signal: AbortSignal.timeout(15000),
        headers: cached?.etag ? { "If-None-Match": cached.etag } : {},
      });
      if (sequence !== state.viewSequence) return;
      if (!response.ok && response.status !== 304)
        throw new Error(`Could not load saved ${state.view}.`);
      const data =
        response.status === 304 ? cached.data : await response.json();
      if (sequence !== state.viewSequence) return;
      if (response.status !== 304) {
        if (state.cache.size >= 12) state.cache.clear();
        state.cache.set(path, { etag: response.headers.get("ETag"), data });
      }
      state.next = data.next_cursor || "";
      render(data.items);
    } catch (error) {
      showError(error.message);
    }
  }
  function render(items) {
    const catalogue = state.view === "catalogue";
    $("filters").hidden = !catalogue;
    $("view-note").textContent = catalogue
      ? "50 catalogue records per page. Filters apply to the current page."
      : state.view === "changes"
        ? "Accepted changes for the latest update. Ignored deliveries do not create change entries."
        : state.view === "issues"
          ? "Newer valid supplier details resolve product issues. Original update results stay saved. Unidentified records need a supplier correction with the displayed source identity."
          : "Saved update outcomes. Select an update to inspect its changes.";
    $("table-caption").textContent = {
      catalogue: "Saved catalogue",
      changes: "Committed changes",
      issues: "Supplier record issues",
      history: "Update history",
    }[state.view];
    headers(
      catalogue
        ? ["Product", "Price", "Status"]
        : state.view === "changes"
          ? ["Product", "Change", "Old → new"]
          : state.view === "issues"
            ? ["Record", "Problem", "Available action"]
            : ["Update", "Result", "Records", "Saved at"],
    );
    const rows = [];
    for (const item of items) {
      const tr = document.createElement("tr");
      if (catalogue)
        tr.append(
          cell(productName(item)),
          cell(money(item.price), "money"),
          cell(pill(item.active ? "Active" : "Withdrawn", !item.active)),
        );
      else if (state.view === "changes")
        tr.append(
          cell(productName(item.after)),
          cell(item.kind),
          cell(difference(item)),
        );
      else if (state.view === "issues") {
        const record = document.createElement("span");
        record.append(productName(item.after));
        const identity = document.createElement("span");
        identity.className = "product-id";
        identity.textContent = `${item.update_id}/${item.source_id}`;
        record.append(identity);
        tr.append(
          cell(record),
          cell(item.problem),
          cell(
            item.resolved
              ? "Resolved by newer supplier details"
              : "Ask supplier for a corrected record",
            item.resolved ? "muted" : "",
          ),
        );
      } else {
        const inspect = document.createElement("button");
        inspect.textContent = item.id.slice(0, 13);
        inspect.setAttribute("aria-label", `Inspect update ${item.id}`);
        inspect.addEventListener("click", () => {
          state.inspectedUpdate = item.id;
          switchView("changes");
        });
        tr.append(
          cell(inspect),
          cell(
            pill(
              labels[item.status] || item.status,
              ["interrupted", "finished_with_issues"].includes(item.status),
            ),
          ),
          cell(`${item.processed} / ${item.total}`),
          cell(new Date(item.updated_at).toLocaleString()),
        );
      }
      rows.push(tr);
    }
    $("table-body").replaceChildren(...rows);
    $("empty").hidden = items.length > 0;
    $("previous").disabled = state.pages.length === 0;
    $("next").disabled = !state.next;
    $("page-number").textContent = `Page ${state.pages.length + 1}`;
  }
  function switchView(view) {
    state.view = view;
    state.cursor = "";
    state.pages = [];
    state.next = "";
    for (const button of document.querySelectorAll("[role=tab]")) {
      const selected = button.id === `tab-${view}`;
      button.setAttribute("aria-selected", String(selected));
      button.tabIndex = selected ? 0 : -1;
    }
    $("catalogue-panel").setAttribute("aria-labelledby", `tab-${view}`);
    loadView();
  }
  $("start").addEventListener("click", () =>
    action("/api/updates/start", { scenario: $("scenario").value }),
  );
  $("resume").addEventListener("click", () =>
    action(`/api/updates/${state.job.id}/resume`, {}, false),
  );
  $("restore").addEventListener("click", () =>
    action("/api/demo/restore", { update_id: state.job.id }, false),
  );
  $("publish").addEventListener("click", () =>
    action("/api/demo/publish", { scenario: $("scenario").value }),
  );
  $("replay").addEventListener("click", () =>
    action("/api/updates/start", {
      scenario: state.job.scenario,
      update_id: state.job.id,
    }),
  );
  $("reset").addEventListener("click", () => {
    if (
      confirm(
        "Restore a fresh 10,000-product synthetic baseline and clear previous demo records? An interrupted update will be abandoned.",
      )
    )
      action("/api/demo/reset", { confirm: "reset synthetic demo" });
  });
  $("refresh").addEventListener("click", async () => {
    await status();
    await loadView();
  });
  $("scenario").addEventListener("change", () => {
    $("expected").textContent =
      `Expected: ${expectations[$("scenario").value]}`;
  });
  $("expected").textContent = `Expected: ${expectations.normal}`;
  $("filters").addEventListener("submit", (event) => {
    event.preventDefault();
    loadView();
  });
  $("next").addEventListener("click", () => {
    state.pages.push(state.cursor);
    state.cursor = state.next;
    loadView();
  });
  $("previous").addEventListener("click", () => {
    state.cursor = state.pages.pop() || "";
    loadView();
  });
  const tabs = [...document.querySelectorAll("[role=tab]")];
  for (const [index, button] of tabs.entries()) {
    button.addEventListener("click", () => {
      state.inspectedUpdate = null;
      switchView(button.id.replace("tab-", ""));
    });
    button.addEventListener("keydown", (event) => {
      let next;
      if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
      if (event.key === "ArrowLeft")
        next = (index + tabs.length - 1) % tabs.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = tabs.length - 1;
      if (next !== undefined) {
        event.preventDefault();
        tabs[next].focus();
        tabs[next].click();
      }
    });
  }
  window.addEventListener("pagehide", clearPoll);
  status();
})();
