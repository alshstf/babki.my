import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { RunsTable } from "./runs-table";
import type { TinvestLinkedAccount, TinvestSyncRun } from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
function servePages(pages: { runs: TinvestSyncRun[]; has_more: boolean }[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
    // Paged by offset, so the second press gets the second page.
    const offset = Number(url.searchParams.get("offset") ?? "0");
    const index = pages.findIndex((_, i) => i === (offset === 0 ? 0 : 1));
    const page = pages[index] ?? { runs: [], has_more: false };
    return Promise.resolve(
      new Response(JSON.stringify(page), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

function serveRuns(runs: TinvestSyncRun[], hasMore = false) {
  servePages([{ runs, has_more: hasMore }]);
}

const LINKS: TinvestLinkedAccount[] = [
  {
    link_id: "link-1",
    account_id: "acc-1",
    broker_account_id: "b-1",
    broker_account_name: "Брокерский счёт",
    broker_account_type: "ACCOUNT_TYPE_TINKOFF",
    opened_on: "2020-01-01",
  },
];

function makeRun(overrides: Partial<TinvestSyncRun> = {}): TinvestSyncRun {
  return {
    id: "run-1",
    link_id: "link-1",
    trigger: "schedule",
    status: "ok",
    started_at: "2026-08-04T09:15:00Z",
    finished_at: "2026-08-04T09:16:00Z",
    read_count: 120,
    added_count: 3,
    disappeared_count: 1,
    unparsed_count: 2,
    error: "",
    reconcile_status: "matched",
    reconciled_at: "2026-08-04T09:16:00Z",
    mismatches: [],
    ...overrides,
  };
}

function renderTable() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RunsTable connectionId="conn-1" links={LINKS} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  fetchMock.mockReset();
  serveRuns([]);
});

describe("RunsTable — what a run is allowed to report", () => {
  it("shows all four figures for a run that finished", async () => {
    serveRuns([makeRun()]);
    renderTable();

    expect(await screen.findByText("Прочитано у брокера: 120")).toBeInTheDocument();
    expect(screen.getByText("Новых: 3")).toBeInTheDocument();
    expect(screen.getByText("Брокер перестал отдавать: 1")).toBeInTheDocument();
    expect(screen.getByText("Не разобрано всего по этому счёту: 2")).toBeInTheDocument();
    expect(screen.getByText("04.08.2026, 12:15")).toBeInTheDocument();
    expect(screen.getByText("Брокерский счёт")).toBeInTheDocument();
    expect(screen.getByText("По расписанию")).toBeInTheDocument();
  });

  // A failed run's unparsed count may never have been taken (it falls to
  // zero, see syncWorker.unparsedNow); the row shows the cause instead.
  it("shows a failed run's cause instead of an unparsed figure nobody can vouch for", async () => {
    serveRuns([
      makeRun({
        status: "failed",
        error: "tinvest: broker answered 500",
        unparsed_count: 7,
        reconcile_status: "not_checked",
        reconciled_at: null,
      }),
    ]);
    renderTable();

    expect(await screen.findByText("Ошибка")).toBeInTheDocument();
    // The cause itself, not just its label.
    expect(
      screen.getByText("Причина отказа: tinvest: broker answered 500"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Не разобрано всего по этому счёту/)).not.toBeInTheDocument();
    // The three measured figures stay: the rolled-back pass did read them.
    expect(screen.getByText("Прочитано у брокера: 120")).toBeInTheDocument();
  });

  // A running row's counters are column defaults (migration 0014), not
  // measurements.
  it("draws no figures at all for a run that has not finished", async () => {
    serveRuns([
      makeRun({
        status: "running",
        finished_at: null,
        read_count: 0,
        added_count: 0,
        disappeared_count: 0,
        unparsed_count: 0,
        error: "",
        reconcile_status: "not_checked",
        reconciled_at: null,
      }),
    ]);
    renderTable();

    expect(await screen.findByText("Прогон не закончился")).toBeInTheDocument();
    expect(screen.queryByText("Прочитано у брокера: 0")).not.toBeInTheDocument();
    expect(screen.queryByText(/Не разобрано/)).not.toBeInTheDocument();
  });
});

describe("RunsTable — the reconcile column", () => {
  it("says «не проверено» for a run that never checked, and counts nothing there", async () => {
    serveRuns([makeRun({ reconcile_status: "not_checked", reconciled_at: null })]);
    renderTable();

    expect(await screen.findByText("Не проверено")).toBeInTheDocument();
    expect(screen.queryByText(/расхождений/)).not.toBeInTheDocument();
  });

  it("counts the differences only where the empty list would have meant «found none»", async () => {
    serveRuns([
      makeRun({
        reconcile_status: "mismatched",
        mismatches: [
          { kind: "instrument", instrument_id: "i-1", label: "SBER", broker: "1", journal: "2" },
        ],
      }),
    ]);
    renderTable();

    expect(await screen.findByText("Расхождение")).toBeInTheDocument();
    expect(screen.getByText("расхождений: 1")).toBeInTheDocument();
  });
});

describe("RunsTable — the log itself", () => {
  it("says the log is empty rather than drawing an empty table", async () => {
    serveRuns([]);
    renderTable();

    expect(await screen.findByText("Синхронизаций ещё не было")).toBeInTheDocument();
  });

  it("fetches the next page on demand, keeping what is already shown", async () => {
    servePages([
      { runs: [makeRun({ id: "run-1", read_count: 120 })], has_more: true },
      { runs: [makeRun({ id: "run-2", read_count: 7 })], has_more: false },
    ]);
    renderTable();

    fireEvent.click(await screen.findByRole("button", { name: "Показать ещё" }));

    expect(await screen.findByText("Прочитано у брокера: 7")).toBeInTheDocument();
    expect(screen.getByText("Прочитано у брокера: 120")).toBeInTheDocument();
    // The server said there is nothing further, so the button is gone.
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
  });

  it("offers no next page when the server says the log ends here", async () => {
    serveRuns([makeRun()], false);
    renderTable();

    await screen.findByText("Прочитано у брокера: 120");
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
  });
});
