import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import "@/i18n";
import { AccountBusyNotice, BackgroundActivity } from "./background-activity";
import { overallShare, taskShare, type BackgroundTask } from "@/api/background";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const broker = "acc-broker";
const bank = "acc-bank";

const sync: BackgroundTask = {
  id: 1,
  kind: "tinvest.sync",
  stage: "journal",
  done: 412,
  total: 1247,
  account_ids: [broker],
  whole_instance: false,
  started_at: "2026-10-10T09:05:00Z",
};
const rates: BackgroundTask = {
  id: 2,
  kind: "marketdata.backfill_fx",
  stage: "rates",
  done: 3,
  total: 4,
  account_ids: [],
  whole_instance: true,
  started_at: "2026-10-10T09:06:00Z",
};

let running: BackgroundTask[] = [];

function serve() {
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const request = input instanceof Request ? input : new Request(String(input));
    const path = new URL(request.url, "http://localhost").pathname;
    const json = (body: unknown) =>
      new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/api/v1/background-tasks")) return json(running);
    if (path.endsWith("/api/v1/accounts")) {
      return json([
        { id: broker, name: "Т-Брокер" },
        { id: bank, name: "Вклад" },
      ]);
    }
    return new Response("null", { status: 404 });
  });
}

function renderWith(ui: React.ReactNode) {
  serve();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
  return client;
}

// Intl writes non-breaking and narrow spaces.
const text = (el: HTMLElement) => (el.textContent ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
  running = [];
});

describe("taskShare", () => {
  it("runs the import's bar once through its stages rather than restarting at each", () => {
    // Reading the rows is the second of five stages, a third of the way in.
    expect(taskShare(sync)).toBeCloseTo((1 + 412 / 1247) / 5);
    expect(taskShare({ ...sync, stage: "operations", done: 0, total: 4 })).toBe(0);
    expect(taskShare({ ...sync, stage: "reconcile", done: 4, total: 4 })).toBe(1);
  });

  it("tells a stage that cannot be counted from one that is done", () => {
    expect(taskShare({ ...rates, done: 0, total: 0 })).toBeNull();
    expect(taskShare(rates)).toBe(0.75);
    expect(overallShare([{ ...rates, total: 0 }])).toBeNull();
    expect(overallShare([rates, { ...rates, id: 3, done: 1, total: 4 }])).toBe(0.5);
  });
});

describe("BackgroundActivity", () => {
  it("shows nothing while nothing runs", async () => {
    renderWith(<BackgroundActivity />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(screen.queryByTestId("background-indicator")).toBeNull();
  });

  it("folds the running jobs into a count and a share, and opens each with its bar, stage and accounts", async () => {
    running = [sync, rates];
    renderWith(<BackgroundActivity />);

    const indicator = await screen.findByTestId("background-indicator");
    expect(indicator.getAttribute("aria-label")).toBe("Фоновые операции: 2");
    // The import is 27 % through (the second of five stages, a third in), the
    // rates 75 %: 51 % together.
    expect(text(indicator)).toMatch(/^2\s*51 %$/);

    fireEvent.click(indicator);
    const items = await screen.findAllByTestId("background-task");
    expect(items).toHaveLength(2);
    expect(text(items[0])).toContain("Загрузка из Т-Инвестиций");
    expect(text(items[0])).toContain("разбираем операции в журнал — 412 из 1247");
    await waitFor(() => expect(text(items[0])).toContain("Счета: Т-Брокер"));
    expect(text(items[1])).toContain("Курсы ЦБ за прошлые дни");
    expect(text(items[1])).toContain("Для всей программы");
  });

  it("reads the screens again when a job ends", async () => {
    running = [sync];
    const journal = vi.fn(async () => "x");
    function Journal() {
      useQuery({ queryKey: ["positions", broker], queryFn: journal });
      return null;
    }
    const client = renderWith(
      <>
        <BackgroundActivity />
        <Journal />
      </>,
    );
    await screen.findByTestId("background-indicator");
    await waitFor(() => expect(journal).toHaveBeenCalledTimes(1));

    running = [];
    await act(() => client.refetchQueries({ queryKey: ["background-tasks"] }));
    await waitFor(() => expect(screen.queryByTestId("background-indicator")).toBeNull());
    await waitFor(() => expect(journal).toHaveBeenCalledTimes(2));
  });
});

describe("AccountBusyNotice", () => {
  it("tells an account a running job changes that its figures are not final", async () => {
    running = [sync];
    renderWith(<AccountBusyNotice accountId={broker} />);
    const notice = await screen.findByTestId("account-busy-notice");
    expect(text(notice)).toContain("Загрузка из Т-Инвестиций: разбираем операции в журнал — 412 из 1247");
    expect(text(notice)).toContain("ещё не окончательные");
  });

  it("says nothing on an account no job touches", async () => {
    running = [sync, rates];
    renderWith(<AccountBusyNotice accountId={bank} />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(screen.queryByTestId("account-busy-notice")).toBeNull();
  });
});
