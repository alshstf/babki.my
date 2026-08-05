import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ReconcilePanel } from "./reconcile-panel";
import type {
  TinvestReconcileMismatch,
  TinvestReconcileSnapshot,
  TinvestUnparsedOperation,
} from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time, so the double has to
// be installed before the imports above run.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response on every call — a single mockResolvedValue breaks on the
// second one, because a body can only be read once.
function serve(routes: { path: string; status?: number; body?: unknown }[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = input instanceof Request ? input.url : String(input);
    const path = new URL(url, "http://localhost").pathname;
    const match = routes.find((r) => path.endsWith(r.path));
    return Promise.resolve(
      new Response(JSON.stringify(match?.body ?? null), {
        status: match ? (match.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

function serveUnparsed(operations: TinvestUnparsedOperation[], hasMore = false) {
  serve([
    {
      path: "/api/v1/tinvest/connections/conn-1/unparsed",
      body: { operations, has_more: hasMore },
    },
  ]);
}

function makeUnparsed(id: string): TinvestUnparsedOperation {
  return {
    id,
    occurred_at: "2026-08-04T09:15:00Z",
    op_type: "OPERATION_TYPE_FUTURES",
    payment: "-1234.5",
    currency: "RUB",
    description: "",
    reason: "unsupported_type",
    raw: { id: "broker-1" },
  };
}

function renderPanel(snapshot: TinvestReconcileSnapshot | null) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ReconcilePanel connectionId="conn-1" snapshot={snapshot} />
    </QueryClientProvider>,
  );
}

const MISMATCH: TinvestReconcileMismatch = {
  kind: "instrument",
  instrument_id: "instr-1",
  label: "SBER",
  broker: "150",
  journal: "100",
};

beforeEach(() => {
  fetchMock.mockReset();
  serveUnparsed([]);
});

describe("ReconcilePanel — the three verdicts are three different things", () => {
  it("says «не проверено» for a connection nothing ever reconciled, and does not say it agrees", async () => {
    renderPanel(null);

    expect(await screen.findByText("Не проверено")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Ни один прогон ещё не сверял посчитанное здесь с тем, что говорит о себе брокер. Это не то же самое, что «расхождений нет»",
      ),
    ).toBeInTheDocument();
    // The two claims a never-made check must not produce.
    expect(screen.queryByText("Сходится с брокером")).not.toBeInTheDocument();
    expect(screen.queryByText("Расходится с брокером")).not.toBeInTheDocument();
  });

  it("says it agrees — and when it was checked — only for a matched snapshot", async () => {
    renderPanel({ at: "2026-08-04T09:15:00Z", status: "matched", mismatches: [] });

    expect(await screen.findByText("Сходится с брокером")).toBeInTheDocument();
    expect(screen.getByText("Проверено 04.08.2026, 12:15")).toBeInTheDocument();
    expect(screen.queryByText("Не проверено")).not.toBeInTheDocument();
  });

  it("says it disagrees, and shows both figures side by side", async () => {
    renderPanel({ at: "2026-08-04T09:15:00Z", status: "mismatched", mismatches: [MISMATCH] });

    expect(await screen.findByText("Расходится с брокером")).toBeInTheDocument();
    expect(screen.getByText("SBER")).toBeInTheDocument();
    // Both sides, never their difference: a reader who sees only the gap
    // cannot tell which side to go and look at.
    expect(screen.getByText("150")).toBeInTheDocument();
    expect(screen.getByText("100")).toBeInTheDocument();
    expect(screen.queryByText("Не проверено")).not.toBeInTheDocument();
  });

  // The contract says a snapshot is built only from runs that made a check, so
  // this shape cannot arrive. If it ever did, the status is what the panel
  // believes — the object's mere presence is not a check.
  it("believes a snapshot's own «not_checked» over the fact that a snapshot exists", async () => {
    renderPanel({ at: "2026-08-04T09:15:00Z", status: "not_checked", mismatches: [] });

    expect(await screen.findByText("Не проверено")).toBeInTheDocument();
    expect(screen.queryByText("Сходится с брокером")).not.toBeInTheDocument();
    expect(screen.queryByText("Проверено 04.08.2026, 12:15")).not.toBeInTheDocument();
  });
});

describe("ReconcilePanel — the kinds of difference read differently", () => {
  it("sends a security difference to the unparsed list, and says an unsupported asset is not one", async () => {
    renderPanel({
      at: "2026-08-04T09:15:00Z",
      status: "mismatched",
      mismatches: [
        MISMATCH,
        { kind: "currency", instrument_id: null, label: "USD", broker: "10", journal: "12" },
        {
          kind: "unsupported",
          instrument_id: null,
          label: "Si-9.26",
          broker: "3",
          journal: "0",
        },
      ],
    });

    expect(await screen.findByText("Расхождение по бумаге")).toBeInTheDocument();
    expect(screen.getByText("Расхождение по деньгам")).toBeInTheDocument();
    expect(screen.getByText("Актив, который программа не учитывает")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "У брокера есть активы, которых эта программа не ведёт вовсе — например, фьючерсы и опционы. Это граница программы, а не сбой загрузки: повторная синхронизация ничего тут не изменит",
      ),
    ).toBeInTheDocument();
  });

  it("does not blame the unparsed list when nothing differs about a security", async () => {
    renderPanel({
      at: "2026-08-04T09:15:00Z",
      status: "mismatched",
      mismatches: [
        { kind: "currency", instrument_id: null, label: "USD", broker: "10", journal: "12" },
      ],
    });

    expect(await screen.findByText("Расхождение по деньгам")).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).not.toBeInTheDocument();
  });
});

describe("ReconcilePanel — the unparsed counter says only what was counted", () => {
  it("gives the exact figure when the server says there is no more behind the page", async () => {
    serveUnparsed([makeUnparsed("u-1"), makeUnparsed("u-2")]);
    renderPanel(null);

    expect(await screen.findByText("Неразобранных операций: 2")).toBeInTheDocument();
  });

  it("publishes a floor, not a total, while the server says there is more", async () => {
    serveUnparsed([makeUnparsed("u-1"), makeUnparsed("u-2")], true);
    renderPanel(null);

    expect(await screen.findByText("Неразобранных операций: не меньше 2")).toBeInTheDocument();
    expect(screen.queryByText("Неразобранных операций: 2")).not.toBeInTheDocument();
  });

  it("says there are none when the list is empty", async () => {
    serveUnparsed([]);
    renderPanel(null);

    expect(await screen.findByText("Неразобранных операций нет")).toBeInTheDocument();
  });

  it("says it does not know rather than drawing a zero, when the list could not be read", async () => {
    serve([{ path: "/api/v1/tinvest/connections/conn-1/unparsed", status: 500, body: {} }]);
    renderPanel(null);

    expect(
      await screen.findByText("Сколько операций осталось неразобранными, узнать не удалось"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Неразобранных операций нет")).not.toBeInTheDocument();
  });
});
