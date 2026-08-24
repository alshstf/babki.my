import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CorporateActions } from "./corporate-actions";
import type { InstrumentEvent } from "@/api/instrument-events";

// openapi-fetch captures globalThis.fetch at import time, so the double has to
// be installed before the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

type Route = { path: string; method?: string; status?: number; body?: unknown };

// A fresh Response per call: mockResolvedValue's single object breaks on the
// second, since a body can only be read once.
function serve(routes: Route[]) {
  fetchMock.mockImplementation(
    (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method =
        (input instanceof Request ? input.method : init?.method) ?? "GET";
      const path = new URL(url, "http://localhost").pathname;
      const match = routes.find(
        (r) =>
          path.startsWith(r.path) &&
          (r.method ?? "GET").toUpperCase() === method.toUpperCase(),
      );
      const status = match ? (match.status ?? 200) : 404;
      return Promise.resolve(
        new Response(JSON.stringify(match?.body ?? null), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      );
    },
  );
}

async function bodiesSent(method: string): Promise<Record<string, unknown>[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const sent =
      (input instanceof Request
        ? input.method
        : (init as RequestInit | undefined)?.method) ?? "GET";
    return sent.toUpperCase() === method.toUpperCase();
  });
  return Promise.all(
    calls.map(async ([input, init]) => {
      const raw =
        input instanceof Request
          ? await input.clone().text()
          : String((init as RequestInit).body);
      return JSON.parse(raw) as Record<string, unknown>;
    }),
  );
}

function makeEvent(overrides: Partial<InstrumentEvent> = {}): InstrumentEvent {
  return {
    id: "event-1",
    kind: "split",
    isin: "US0231351067",
    effective_on: "2022-06-06",
    ratio_from: 1,
    ratio_to: 20,
    source: "manual",
    source_ref: "https://ir.aboutamazon.com/",
    note: "",
    materialized: true,
    created_at: "2026-08-24T00:00:00Z",
    ...overrides,
  };
}

function renderSection(canEdit = true) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <CorporateActions canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  fetchMock.mockReset();
});

describe("CorporateActions", () => {
  it("shows the evidence behind a recorded event", async () => {
    // A ratio nobody can check is a number this program carries into every
    // holder's journal on one person's word, so the link is required when the
    // event is recorded — and shown, so it can actually be followed.
    serve([
      { path: "/api/v1/instrument-events", body: { events: [makeEvent()] } },
    ]);

    renderSection();

    expect(await screen.findByTestId("corporate-action-row")).toBeInTheDocument();
    const link = screen.getByTestId("corporate-action-source");
    expect(link).toHaveAttribute("href", "https://ir.aboutamazon.com/");
    expect(screen.getByText("1:20")).toBeInTheDocument();
  });

  it("sends what was typed, and only the fields the kind uses", async () => {
    serve([
      { path: "/api/v1/instrument-events", body: { events: [] } },
      {
        path: "/api/v1/instrument-events",
        method: "POST",
        status: 201,
        body: {
          event: makeEvent(),
          rows_added: 1,
          rows_removed: 0,
          accounts_touched: 1,
          recheck_queued: 1,
        },
      },
    ]);

    renderSection();
    fireEvent.click(await screen.findByTestId("corporate-action-add"));

    fireEvent.change(screen.getByLabelText("ISIN бумаги"), {
      target: { value: "us0231351067" },
    });
    fireEvent.change(screen.getByLabelText("Дата вступления"), {
      target: { value: "2022-06-06" },
    });
    fireEvent.change(screen.getByLabelText("Коэффициент"), {
      target: { value: "1" },
    });
    fireEvent.change(screen.getByLabelText("Стало"), {
      target: { value: "20" },
    });
    fireEvent.change(screen.getByLabelText("Ссылка на подтверждение"), {
      target: { value: "https://ir.aboutamazon.com/" },
    });
    fireEvent.click(screen.getByText("Сохранить"));

    await waitFor(async () => {
      expect(await bodiesSent("POST")).toHaveLength(1);
    });
    const [sent] = await bodiesSent("POST");
    expect(sent).toEqual({
      kind: "split",
      // Uppercased on the way out: the ISIN is an identity, and the registry
      // matches on it exactly.
      isin: "US0231351067",
      effective_on: "2022-06-06",
      ratio_from: 1,
      ratio_to: 20,
      source_ref: "https://ir.aboutamazon.com/",
    });
    // A split produces no second paper and moves no basis, and the server
    // refuses either field on one — so a form left alone must not send them.
    expect(sent).not.toHaveProperty("result_isin");
    expect(sent).not.toHaveProperty("basis_share");
  });

  it("offers no delete on the exchange's own row", async () => {
    // The job that wrote it reads the exchange's table on every run and would
    // write it back, so the button would undo itself.
    serve([
      {
        path: "/api/v1/instrument-events",
        body: {
          events: [
            makeEvent({
              id: "moex-1",
              source: "moex_iss",
              source_ref: "https://iss.moex.com/",
            }),
          ],
        },
      },
    ]);

    renderSection();

    expect(await screen.findByTestId("corporate-action-row")).toBeInTheDocument();
    expect(screen.queryByTestId("corporate-action-delete-moex-1")).toBeNull();
    // The source line reads «Московская биржа · Источник», so the text is
    // matched inside its line rather than as a whole node.
    expect(
      screen.getByText((_, el) => el?.textContent?.startsWith("Московская биржа") === true, {
        selector: "div",
      }),
    ).toBeInTheDocument();
  });

  it("says on the row itself when a kind is recorded but not yet counted", async () => {
    // The facts are perishable — a fund converted in 2023 and nobody can go
    // back and ask the registrar again — so they are stored before the engine
    // can fold them, and the row says so rather than looking like a holding
    // that silently did not change.
    serve([
      {
        path: "/api/v1/instrument-events",
        body: {
          events: [
            makeEvent({
              id: "conv-1",
              kind: "conversion",
              result_isin: "RU000A107UL4",
              materialized: false,
            }),
          ],
        },
      },
    ]);

    renderSection();

    expect(await screen.findByText("Пока не учитывается в журнале")).toBeInTheDocument();
    expect(screen.getByText("US0231351067 → RU000A107UL4")).toBeInTheDocument();
  });

  it("offers nothing to write to a member who may not", async () => {
    serve([
      { path: "/api/v1/instrument-events", body: { events: [makeEvent()] } },
    ]);

    renderSection(false);

    expect(await screen.findByTestId("corporate-action-row")).toBeInTheDocument();
    expect(screen.queryByTestId("corporate-action-add")).toBeNull();
    expect(screen.queryByTestId("corporate-action-delete-event-1")).toBeNull();
  });
});
