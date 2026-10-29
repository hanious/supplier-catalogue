"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const api = window.CATALOGUE_API || "";
  const scenarioStorageKey = "supplier-catalogue.scenario";
  const attentionStorageKey = "supplier-catalogue.attention";
  const state = {
    job: null,
    inspectedUpdate: null,
    cleanupPending: false,
    baselineReady: false,
    generation: null,
    dataRevision: null,
    epoch: null,
    view: "catalogue",
    page: 1,
    pageCount: 0,
    total: 0,
    timer: null,
    busy: false,
    pending: null,
    cache: new Map(),
    viewSequence: 0,
    attention: { scope: "", seen: { changes: 0, issues: 0 } },
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
      "Save 30 of 100 records, then interrupt after three failed supplier requests. Restore supplier, then Resume update; saved records stay saved.",
    invalid:
      "Save 2 valid changes, show 2 issues, and finish with issues. Existing valid prices stay saved.",
    slow: "Import 100 records slowly, in ten batches. Close and reopen this page; processing continues and saved progress reloads.",
    full: "Import all 10,000 records, with durable progress and a terminal supplier marker. Duration depends on storage throughput.",
    correction:
      "Send newer valid details for two records. A prior product issue resolves; an unidentified record still needs a supplier correction with its source identity.",
  };
  const views = {
    catalogue: {
      sorts: [
        ["id", "Product ID"],
        ["name", "Product name"],
        ["price", "Price"],
        ["status", "Status"],
      ],
      filters: [
        ["", "All products"],
        ["active", "Active"],
        ["withdrawn", "Withdrawn"],
      ],
      order: "asc",
    },
    changes: {
      sorts: [
        ["source_id", "Record ID"],
        ["name", "Product name"],
        ["kind", "Change type"],
      ],
      filters: [
        ["", "All changes"],
        ["added", "Added"],
        ["changed", "Changed"],
        ["withdrawn", "Withdrawn"],
      ],
      order: "asc",
    },
    issues: {
      sorts: [
        ["source_id", "Record ID"],
        ["name", "Product name"],
        ["status", "Resolution"],
      ],
      filters: [
        ["", "All issues"],
        ["open", "Needs correction"],
        ["resolved", "Resolved"],
      ],
      order: "asc",
    },
    history: {
      sorts: [
        ["created_at", "Started at"],
        ["status", "Result"],
        ["processed", "Records processed"],
      ],
      filters: [
        ["", "All updates"],
        ...Object.entries(labels).map(([value, label]) => [value, label]),
      ],
      order: "desc",
    },
  };
  function updateScenarioHint() {
    $("expected").textContent =
      `Expected: ${expectations[$("scenario").value]}`;
  }
  function restoreScenarioChoice() {
    let choice = "normal";
    try {
      const saved = localStorage.getItem(scenarioStorageKey);
      if (Object.hasOwn(expectations, saved)) choice = saved;
    } catch {
      // Browser storage can be unavailable; demo controls must still work.
    }
    $("scenario").value = choice;
    updateScenarioHint();
  }
  function updateAttention() {
    const j = state.job;
    const scope =
      j && j.scenario !== "baseline" ? `${state.epoch}/${j.id}` : "";
    const totals = {
      changes: j ? j.counts.added + j.counts.changed + j.counts.withdrawn : 0,
      issues: j?.counts.issues || 0,
    };
    if (state.attention.scope !== scope) {
      const seen = { changes: 0, issues: 0 };
      try {
        const saved = JSON.parse(localStorage.getItem(attentionStorageKey));
        if (saved?.scope === scope) {
          for (const view of Object.keys(seen)) {
            const count = saved.seen?.[view];
            if (Number.isSafeInteger(count) && count >= 0 && count <= 10000)
              seen[view] = count;
          }
        }
      } catch {
        // Invalid or unavailable browser storage cannot block notifications.
      }
      state.attention = { scope, seen };
    }
    let acknowledged = false;
    for (const view of ["changes", "issues"]) {
      const currentView =
        state.view === view &&
        (view !== "changes" ||
          !state.inspectedUpdate ||
          state.inspectedUpdate === j?.id);
      if (scope && currentView && totals[view] > state.attention.seen[view]) {
        state.attention.seen[view] = totals[view];
        acknowledged = true;
      }
      const unread = scope
        ? Math.max(0, totals[view] - state.attention.seen[view])
        : 0;
      const badge = $(`badge-${view}`);
      badge.hidden = unread === 0;
      badge.textContent = unread.toLocaleString();
      const notice = `${unread.toLocaleString()} new ${unread === 1 ? view.slice(0, -1) : view} since last opened`;
      badge.title = notice;
      $(`notice-${view}`).textContent = unread ? notice : "";
    }
    if (acknowledged) {
      try {
        localStorage.setItem(
          attentionStorageKey,
          JSON.stringify(state.attention),
        );
      } catch {
        // Viewed counts still work for this page when persistence is unavailable.
      }
    }
  }
  function configureFilters() {
    const config = views[state.view];
    for (const [id, options] of [
      ["sort-by", config.sorts],
      ["filter-status", config.filters],
    ]) {
      $(id).replaceChildren(
        ...options.map(([value, label]) => {
          const option = document.createElement("option");
          option.value = value;
          option.textContent = label;
          return option;
        }),
      );
    }
    $("sort-order").value = config.order;
    $("search").value = "";
  }
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
  function pill(value, tone = "info") {
    const span = document.createElement("span");
    span.className = "pill";
    span.dataset.tone = tone;
    span.textContent = value;
    return span;
  }
  function productName(p) {
    const span = document.createElement("span");
    span.className = "product-name";
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
    const interrupted = state.job?.status === "interrupted";
    const resetting = state.job?.scenario === "baseline" && (active || interrupted);
    $("start").disabled =
      state.busy ||
      !state.baselineReady ||
      active ||
      interrupted;
    $("resume").hidden = !interrupted;
    $("resume").disabled = state.busy;
    $("reset").disabled = state.busy || active || (state.cleanupPending && !interrupted);
    $("restore").disabled = state.busy || !state.job;
    $("publish").disabled = state.busy || resetting;
    $("replay").disabled = state.busy || !state.job || active;
  }
  async function status() {
    clearPoll();
    try {
      const data = await request("/api/status");
      const previous = state.job;
      const changed =
        state.generation !== data.generation ||
        state.dataRevision !== data.data_revision ||
        state.epoch !== data.epoch ||
        previous?.status !== data.job?.status ||
        previous?.id !== data.job?.id ||
        (state.view !== "catalogue" &&
          previous?.processed !== data.job?.processed);
      state.job = data.job;
      state.generation = data.generation;
      state.dataRevision = data.data_revision;
      state.epoch = data.epoch;
      state.cleanupPending = data.cleanup_pending;
      state.baselineReady = data.baseline_ready;
      const j = state.job;
      const hasIssues = (j?.counts.issues || 0) > 0;
      const needsAttention =
        hasIssues ||
        ["interrupted", "unavailable", "finished_with_issues"].includes(
          j?.status,
        );
      const receipt = document.querySelector(".receipt");
      receipt.dataset.state = j?.status || "";
      receipt.dataset.attention = String(needsAttention);
      $("update-warning-icon").hidden = !needsAttention;
      $("update-title").textContent = j
        ? `${j.id.slice(0, 13)} · ${labels[j.status] || j.status}`
        : "Ready for an update";
      $("message").textContent = j
        ? j.message ||
          `${j.processed.toLocaleString()} / ${j.total.toLocaleString()} records processed · saved changes are visible now.`
        : data.baseline_ready
          ? "100-product baseline loaded. Choose a scenario, then start an update."
          : "Use Reset demo to load the 100-product baseline before starting a scenario.";
      if (j?.status === "interrupted")
        $("message").textContent = `Update paused. ${$("message").textContent}`;
      const issueMessage = hasIssues
        ? `${j.counts.issues.toLocaleString()} ${j.counts.issues === 1 ? "record needs" : "records need"} correction. Open the Issues tab for details.`
        : "";
      $("issue-warning").textContent = issueMessage;
      $("issue-warning").hidden = !hasIssues;
      updateAttention();
      $("progress").max = j?.total || 100;
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
      if (
        previous?.status !== j?.status ||
        previous?.id !== j?.id ||
        previous?.counts.issues !== j?.counts.issues
      )
        $("announcement").textContent = j
          ? `${labels[j.status] || j.status}. ${issueMessage}`.trim()
          : "Saved catalogue loaded.";
      $("connection").textContent = data.cleanup_pending &&
        !["running", "interrupted"].includes(j?.status)
        ? "Baseline saved · clearing previous demo records. Refresh status before resetting again."
        : "Latest saved catalogue · status checked just now";
      showError("");
      buttons();
      // Table loading must not hold the action lock after saved status is known.
      if (changed) void loadView();
      if (
        j?.status === "running" ||
        (state.cleanupPending && j?.status !== "interrupted")
      )
        state.timer = setTimeout(status, 2000);
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
        state.page = 1;
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
    for (const title of ["No.", ...titles]) {
      const th = document.createElement("th");
      th.scope = "col";
      th.textContent = title;
      if (title === "No.") th.setAttribute("aria-label", "Item number");
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
    const query = new URLSearchParams({
      page: String(state.page),
      limit: "50",
      q: $("search").value,
      status: $("filter-status").value,
      sort: $("sort-by").value,
      order: $("sort-order").value,
    });
    let endpoint = state.view === "catalogue" ? "products" : state.view;
    if (state.view === "changes") {
      if (!state.job && !state.inspectedUpdate) {
        render({
          items: [],
          page: 0,
          total_pages: 0,
          total_items: 0,
          page_size: 50,
        });
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
      render(data);
    } catch (error) {
      showError(error.message);
    }
  }
  function render(data) {
    const items = data.items;
    state.page = data.page || 1;
    state.pageCount = data.total_pages;
    state.total = data.total_items;
    const catalogue = state.view === "catalogue";
    $("view-note").textContent = catalogue
      ? `${data.total_items.toLocaleString()} matching products · search, filters and sorting apply across the whole catalogue.`
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
      tr.append(
        cell((state.page - 1) * data.page_size + rows.length + 1, "row-number"),
      );
      if (catalogue) {
        tr.dataset.state = item.active ? "active" : "withdrawn";
        tr.append(
          cell(productName(item)),
          cell(money(item.price), "money"),
          cell(
            pill(
              item.active ? "Active" : "Withdrawn",
              item.active ? "positive" : "neutral",
            ),
          ),
        );
      } else if (state.view === "changes") {
        tr.dataset.state = item.after.active ? "active" : "withdrawn";
        tr.append(
          cell(productName(item.after)),
          cell(item.kind),
          cell(difference(item)),
        );
      } else if (state.view === "issues") {
        tr.dataset.state = item.resolved ? "resolved" : "issue";
        const record = document.createElement("span");
        record.append(productName(item.after));
        const identity = document.createElement("span");
        identity.className = "product-id";
        identity.textContent = `${item.update_id}/${item.source_id}`;
        record.append(identity);
        const action = document.createElement("div");
        action.className = "issue-action";
        action.append(
          pill(
            item.resolved ? "Resolved" : "Needs correction",
            item.resolved ? "positive" : "attention",
          ),
          text(
            item.resolved
              ? "Resolved by newer supplier details"
              : "Ask supplier for a corrected record",
          ),
        );
        tr.append(cell(record), cell(item.problem), cell(action));
      } else {
        tr.dataset.state = item.status;
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
              ["interrupted", "unavailable"].includes(item.status)
                ? "warning"
                : item.status === "finished_with_issues"
                  ? "attention"
                  : item.status === "completed"
                    ? "positive"
                    : item.status === "abandoned"
                      ? "neutral"
                      : "info",
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
    $("previous").disabled = state.page <= 1 || state.total === 0;
    $("next").disabled = state.page >= state.pageCount;
    $("page-number").textContent =
      `Page ${data.page} of ${data.total_pages} · ${data.total_items.toLocaleString()} records`;
  }
  function switchView(view) {
    state.view = view;
    state.page = 1;
    configureFilters();
    for (const button of document.querySelectorAll("[role=tab]")) {
      const selected = button.id === `tab-${view}`;
      button.setAttribute("aria-selected", String(selected));
      button.tabIndex = selected ? 0 : -1;
    }
    $("catalogue-panel").setAttribute("aria-labelledby", `tab-${view}`);
    updateAttention();
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
        "Restore a fresh 100-product synthetic baseline and clear previous demo records? An interrupted update will be abandoned.",
      )
    )
      action("/api/demo/reset", { confirm: "reset synthetic demo" });
  });
  $("refresh").addEventListener("click", async () => {
    await status();
    await loadView();
  });
  $("scenario").addEventListener("change", () => {
    try {
      localStorage.setItem(scenarioStorageKey, $("scenario").value);
    } catch {
      // Saving a preference is optional when storage is blocked or full.
    }
    updateScenarioHint();
  });
  $("filters").addEventListener("submit", (event) => {
    event.preventDefault();
    state.page = 1;
    loadView();
  });
  $("next").addEventListener("click", () => {
    state.page++;
    loadView();
  });
  $("previous").addEventListener("click", () => {
    state.page = Math.max(1, state.page - 1);
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
  if (window.matchMedia("(max-width: 900px)").matches)
    $("demo-controls").open = false;
  restoreScenarioChoice();
  configureFilters();
  buttons();
  status();
})();
