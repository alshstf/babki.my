import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { ReconcilePanel } from "./reconcile-panel";
import type {
  TinvestAccountReconcile,
  TinvestReconcileMismatch,
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

// The accounts list answers too: the panel names the babki account each verdict
// belongs to, the same way the connection's list of accounts does.
const ACCOUNTS = [
  { id: "acc-1", name: "Т-Инвестиции: брокерский" },
  { id: "acc-2", name: "Т-Инвестиции: ИИС" },
];

function serveUnparsed(
  operations: TinvestUnparsedOperation[],
  hasMore = false,
) {
  serve([
    { path: "/api/v1/accounts", body: ACCOUNTS },
    {
      path: "/api/v1/tinvest/connections/conn-1/unparsed",
      body: { operations, has_more: hasMore },
    },
  ]);
}

function makeUnparsed(id: string): TinvestUnparsedOperation {
  return {
    id,
    link_id: "link-1",
    content_key: `key-${id}`,
    occurred_at: "2026-08-04T09:15:00Z",
    op_type: "OPERATION_TYPE_FUTURES",
    class_code: "",
    trading_mode_kind: null,
    payment: "-1234.5",
    currency: "RUB",
    description: "",
    reason: "unsupported_type",
    detail: 'broker operation type "OPERATION_TYPE_FUTURES"',
    raw: { id: "broker-1" },
  };
}

// The verdict of one account, with the fields a caller does not care about
// filled in. `link_id`/`account_id` are what tell two of them apart.
function makeReconcile(
  overrides: Partial<TinvestAccountReconcile> = {},
): TinvestAccountReconcile {
  return {
    link_id: "link-1",
    account_id: "acc-1",
    broker_account_name: "Брокерский счёт",
    at: "2026-08-04T09:15:00Z",
    status: "matched",
    mismatches: [],
    currency_trades_unparsed: 0,
    ...overrides,
  };
}

// The panel links to an account, so it is rendered under a router — the same
// pathless "app" layout the production tree nests these screens in.
function renderPanel(reconciles: TinvestAccountReconcile[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => <Outlet />,
  });
  const panelRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/",
    component: () => (
      <ReconcilePanel connectionId="conn-1" reconciles={reconciles} />
    ),
  });
  const accountRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/accounts/$accountId",
    component: () => <div>ACCOUNT</div>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      layoutRoute.addChildren([panelRoute, accountRoute]),
    ]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
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

describe("ReconcilePanel — a verdict belongs to one account", () => {
  // THE CASE THAT USED TO LIE. Two accounts, checked in one sync: the first
  // differs, the second agrees a moment later. A single verdict for the
  // connection was the newest of the two, so the screen drew a tick — and the
  // differing account's verdict, being older, could never be shown at all.
  it("shows both accounts' verdicts and claims no agreement for the connection", async () => {
    renderPanel([
      makeReconcile({
        link_id: "link-1",
        account_id: "acc-1",
        broker_account_name: "Брокерский счёт",
        at: "2026-08-04T09:15:00Z",
        status: "mismatched",
        mismatches: [MISMATCH],
      }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        broker_account_name: "ИИС",
        at: "2026-08-04T09:16:00Z",
        status: "matched",
      }),
    ]);

    // Both verdicts, each beside the account it was made for.
    expect(
      await screen.findByText("Расходится с брокером"),
    ).toBeInTheDocument();
    expect(screen.getByText("Сходится с брокером")).toBeInTheDocument();
    expect(screen.getByText("У брокера: Брокерский счёт")).toBeInTheDocument();
    expect(screen.getByText("У брокера: ИИС")).toBeInTheDocument();
    // The differing account's own figures, which the old screen never reached.
    expect(screen.getByText("SBER")).toBeInTheDocument();
    expect(screen.getByText("150")).toBeInTheDocument();
    expect(screen.getByText("100")).toBeInTheDocument();
    // And nothing that says the connection as a whole agrees.
    expect(screen.getByText("Есть расхождения с брокером")).toBeInTheDocument();
    expect(
      screen.queryByText("Все счета сверены и сходятся с брокером"),
    ).not.toBeInTheDocument();
  });

  it("names each account and links it to the babki account it feeds", async () => {
    renderPanel([
      makeReconcile({ link_id: "link-1", account_id: "acc-1" }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        broker_account_name: "ИИС",
        status: "not_checked",
        at: null,
      }),
    ]);

    expect(
      await screen.findByRole("link", { name: "Т-Инвестиции: брокерский" }),
    ).toHaveAttribute("href", "/accounts/acc-1");
    expect(
      screen.getByRole("link", { name: "Т-Инвестиции: ИИС" }),
    ).toHaveAttribute("href", "/accounts/acc-2");
  });

  it("times each check separately and gives an unchecked account no time at all", async () => {
    renderPanel([
      makeReconcile({
        link_id: "link-1",
        account_id: "acc-1",
        at: "2026-08-04T09:15:00Z",
      }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        status: "not_checked",
        at: null,
      }),
    ]);

    expect(
      await screen.findByText("Проверено 04.08.2026, 12:15"),
    ).toBeInTheDocument();
    expect(screen.getAllByText(/^Проверено /)).toHaveLength(1);
    expect(screen.getByText("Не проверено")).toBeInTheDocument();
  });

  // The contract says a time and a `not_checked` verdict cannot arrive
  // together. If they ever did, the verdict is what this panel believes: a
  // rendered timestamp under «Не проверено» would be the screen saying both.
  it("believes an account's own «not_checked» over a time printed beside it", async () => {
    renderPanel([
      makeReconcile({ status: "not_checked", at: "2026-08-04T09:15:00Z" }),
    ]);

    expect(await screen.findByText("Не проверено")).toBeInTheDocument();
    expect(
      screen.queryByText("Проверено 04.08.2026, 12:15"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("Сходится с брокером")).not.toBeInTheDocument();
  });
});

describe("ReconcilePanel — the connection's own line is derived", () => {
  it("says the whole connection agrees only when every account was checked and agreed", async () => {
    renderPanel([
      makeReconcile({ link_id: "link-1", account_id: "acc-1" }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        broker_account_name: "ИИС",
      }),
    ]);

    expect(
      await screen.findByText("Все счета сверены и сходятся с брокером"),
    ).toBeInTheDocument();
    expect(screen.queryByText("Сверены не все счета")).not.toBeInTheDocument();
  });

  it("does not call it agreement when one account was never checked", async () => {
    renderPanel([
      makeReconcile({ link_id: "link-1", account_id: "acc-1" }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        broker_account_name: "ИИС",
        status: "not_checked",
        at: null,
      }),
    ]);

    expect(await screen.findByText("Сверены не все счета")).toBeInTheDocument();
    expect(
      screen.queryByText("Все счета сверены и сходятся с брокером"),
    ).not.toBeInTheDocument();
  });

  it("says nobody has checked anywhere, and does not say it agrees", async () => {
    renderPanel([
      makeReconcile({
        link_id: "link-1",
        account_id: "acc-1",
        status: "not_checked",
        at: null,
      }),
    ]);

    expect(
      await screen.findByText("Сверки с брокером ещё не было"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Ни один прогон ещё не сверял посчитанное здесь с тем, что говорит о себе брокер. Это не то же самое, что «расхождений нет»",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Все счета сверены и сходятся с брокером"),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText("Есть расхождения с брокером"),
    ).not.toBeInTheDocument();
  });

  // A difference found on one account is not a report on the others.
  it("adds that some accounts were not checked at all beside a difference", async () => {
    renderPanel([
      makeReconcile({
        link_id: "link-1",
        account_id: "acc-1",
        status: "mismatched",
        mismatches: [MISMATCH],
      }),
      makeReconcile({
        link_id: "link-2",
        account_id: "acc-2",
        broker_account_name: "ИИС",
        status: "not_checked",
        at: null,
      }),
    ]);

    expect(
      await screen.findByText("Есть расхождения с брокером"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Часть счетов при этом не сверялась вовсе — что с ними, отсюда не видно",
      ),
    ).toBeInTheDocument();
  });

  it("says there is nothing to reconcile when there are no accounts", async () => {
    renderPanel([]);

    expect(
      await screen.findByText(
        "Сверять нечего: у этого подключения нет связанных счетов",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Все счета сверены и сходятся с брокером"),
    ).not.toBeInTheDocument();
  });
});

describe("ReconcilePanel — the kinds of difference read differently", () => {
  it("gives a paper the journal never saw its own sentence, not the one about unparsed operations", async () => {
    // The owner's own case: the broker reports TECH2, the fund his TECH was
    // converted into under a new ISIN. Nothing of ours pairs with it, and the
    // row used to read exactly like a paper both sides know but count
    // differently — the only clue being that the label was not one of his
    // tickers. That is a thing to notice rather than a thing to be told.
    serveUnparsed([makeUnparsed("u-1")]);
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [
          {
            kind: "unknown_security",
            instrument_id: null,
            label: "TECH2",
            broker: "60795",
            journal: "0",
          },
        ],
      }),
    ]);

    expect(
      await screen.findByText("Бумага, которой нет в журнале"),
    ).toBeInTheDocument();
    expect(
      screen.getByTestId("reconcile-unknown-security-note").textContent,
    ).toBe(
      "У брокера есть бумаги, о которых в журнале нет ни одной операции, — поэтому сопоставить их не с чем, и в таблице показано название брокера, а не тикер из вашего каталога. Обычно так выглядит корпоративное действие: фонд превратили в другой под новым ISIN, и операцией это не приходит. Что именно произошло с бумагой, знаете только вы — это вносится вручную",
    );
    // And NOT the sentence about unparsed operations: there is an unparsed row
    // in this fixture, so that sentence would have printed if the kind were
    // read as a plain security difference — sending the reader to a list that
    // has nothing to do with a fund conversion.
    expect(
      screen.queryByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).not.toBeInTheDocument();
  });

  it("sends a security difference to the unparsed list when there is one to send it to", async () => {
    serveUnparsed([makeUnparsed("u-1")]);
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [
          MISMATCH,
          {
            kind: "currency",
            instrument_id: null,
            label: "USD",
            broker: "10",
            journal: "12",
          },
          {
            kind: "unsupported",
            instrument_id: null,
            label: "Si-9.26",
            broker: "3",
            journal: "0",
          },
        ],
      }),
    ]);

    expect(
      await screen.findByText("Расхождение по бумаге"),
    ).toBeInTheDocument();
    expect(screen.getByText("Расхождение по деньгам")).toBeInTheDocument();
    expect(
      screen.getByText("Актив, который программа не учитывает"),
    ).toBeInTheDocument();
    expect(
      await screen.findByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "У брокера есть активы, которых эта программа не ведёт вовсе — например, фьючерсы и опционы. Это граница программы, а не сбой загрузки: повторная синхронизация ничего тут не изменит",
      ),
    ).toBeInTheDocument();
  });

  // «ОНИ ПЕРЕЧИСЛЕНЫ НИЖЕ» IS A CLAIM ABOUT THE LIST BELOW. A security
  // difference needs no unreadable operation at all — a plain difference in
  // quantities is one — and the sentence used to be printed directly above
  // «Неразобранных операций нет».
  it("does not promise a list of unreadable operations when there are none", async () => {
    serveUnparsed([]);
    renderPanel([
      makeReconcile({ status: "mismatched", mismatches: [MISMATCH] }),
    ]);

    expect(
      await screen.findByText("Неразобранных операций нет"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).not.toBeInTheDocument();
  });

  it("does not promise one either when the unparsed list could not be read", async () => {
    serve([
      { path: "/api/v1/accounts", body: ACCOUNTS },
      {
        path: "/api/v1/tinvest/connections/conn-1/unparsed",
        status: 500,
        body: {},
      },
    ]);
    renderPanel([
      makeReconcile({ status: "mismatched", mismatches: [MISMATCH] }),
    ]);

    expect(
      await screen.findByText(
        "Сколько операций осталось неразобранными, узнать не удалось",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Расхождение по бумаге чаще всего значит, что часть операций брокера не удалось разобрать: они перечислены ниже",
      ),
    ).not.toBeInTheDocument();
  });

  it("does not blame the unparsed list when nothing differs about a security", async () => {
    serveUnparsed([makeUnparsed("u-1")]);
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [
          {
            kind: "currency",
            instrument_id: null,
            label: "USD",
            broker: "10",
            journal: "12",
          },
        ],
      }),
    ]);

    expect(
      await screen.findByText("Расхождение по деньгам"),
    ).toBeInTheDocument();
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
    renderPanel([makeReconcile({ status: "not_checked", at: null })]);

    expect(
      await screen.findByText("Неразобранных операций: 2"),
    ).toBeInTheDocument();
  });

  it("не считает объяснённые строки, хотя список ниже их показывает", async () => {
    // Одна строка не разобрана, вторая учтена ручной операцией владельца.
    // Счётчик обязан назвать ОДНУ: объяснённая строка — не операция, которую
    // программа не смогла прочитать, и «Неразобранных операций: 2» было бы про
    // неё ложью. Признак берётся из explained_by, а не из пустой причины:
    // пустая причина бывает и у строки, которую ещё пересобирают.
    serveUnparsed([
      makeUnparsed("u-1"),
      {
        ...makeUnparsed("u-2"),
        reason: "" as TinvestUnparsedOperation["reason"],
        detail: "",
        explained_by: {
          id: "exp-1",
          operation_id: "op-1",
          operation_on: "2026-05-21",
          operation_type: "redemption",
        },
      },
    ]);
    renderPanel([makeReconcile({ status: "not_checked", at: null })]);

    expect(
      await screen.findByText("Неразобранных операций: 1"),
    ).toBeInTheDocument();
  });

  it("publishes a floor, not a total, while the server says there is more", async () => {
    serveUnparsed([makeUnparsed("u-1"), makeUnparsed("u-2")], true);
    renderPanel([makeReconcile({ status: "not_checked", at: null })]);

    expect(
      await screen.findByText("Неразобранных операций: не меньше 2"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Неразобранных операций: 2"),
    ).not.toBeInTheDocument();
  });

  it("says there are none when the list is empty", async () => {
    serveUnparsed([]);
    renderPanel([makeReconcile({ status: "not_checked", at: null })]);

    expect(
      await screen.findByText("Неразобранных операций нет"),
    ).toBeInTheDocument();
  });

  it("says it does not know rather than drawing a zero, when the list could not be read", async () => {
    serve([
      { path: "/api/v1/accounts", body: ACCOUNTS },
      {
        path: "/api/v1/tinvest/connections/conn-1/unparsed",
        status: 500,
        body: {},
      },
    ]);
    renderPanel([makeReconcile({ status: "not_checked", at: null })]);

    expect(
      await screen.findByText(
        "Сколько операций осталось неразобранными, узнать не удалось",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Неразобранных операций нет"),
    ).not.toBeInTheDocument();
  });
});

// Расхождение по деньгам, которое не сойдётся никогда, обязано назвать причину:
// иначе оно стоит рядом с расхождениями по бумагам как та же новость, а
// сигнал, который всегда красный, перестают читать.
describe("причина денежного расхождения", () => {
  const cashMismatch = {
    kind: "currency" as const,
    label: "RUB",
    broker: "4862.07",
    journal: "2580881.45",
    instrument_id: null,
  };
  const paperMismatch = {
    kind: "instrument" as const,
    label: "SBER",
    broker: "10",
    journal: "12",
    instrument_id: "instr-1",
  };

  it("названа, когда есть и неразобранные валютные сделки, и расхождение по деньгам", async () => {
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [cashMismatch],
        currency_trades_unparsed: 79,
      }),
    ]);
    const note = await screen.findByTestId("reconcile-currency-trades-note");
    expect(note).toHaveTextContent("79");
  });

  // Обе половины проверяются по отдельности: подпись без своего числа и число
  // без своей подписи — два разных способа соврать.
  it("молчит, когда валютных сделок нет, а деньги всё равно разошлись", async () => {
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [cashMismatch],
        currency_trades_unparsed: 0,
      }),
    ]);
    // Ждём саму строку расхождения, иначе «ничего не нарисовано» прошло бы за
    // «подписи нет» — тест, зеленеющий по неверной причине.
    expect(await screen.findByText("RUB")).toBeInTheDocument();
    expect(screen.queryByTestId("reconcile-currency-trades-note")).toBeNull();
  });

  it("молчит, когда разошлись только бумаги", async () => {
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [paperMismatch],
        currency_trades_unparsed: 79,
      }),
    ]);
    expect(await screen.findByText("SBER")).toBeInTheDocument();
    expect(screen.queryByTestId("reconcile-currency-trades-note")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// Паспорт бумаги, которой нет в каталоге, и кнопка «завести по нему»
// ---------------------------------------------------------------------------

// Фонд, в который превратился TECH: у брокера он есть, в каталоге строки нет,
// и брокер сам говорит, что это за бумага.
const TECH2: TinvestReconcileMismatch = {
  kind: "unknown_security",
  instrument_id: null,
  label: "TECH2",
  broker: "60795",
  journal: "0",
  broker_isin: "RU000A1071G8",
  broker_name: "Заблокированные активы Тинькофф Технологии",
  broker_currency: "RUB",
  broker_type: "etf",
};

// Метод РАЗЛИЧАЕТСЯ: по одному и тому же пути /api/v1/instruments идут и поиск
// (GET), и заведение (POST), и тест, который их путает, доказал бы не то.
// Свежий Response на каждый вызов — общий с serve() резон: тело читается один
// раз, и один mockResolvedValue сломался бы на втором обращении.
function serveCatalog(options: {
  found?: unknown[];
  createStatus?: number;
  syncQueued?: boolean;
  syncStatus?: number;
}) {
  const {
    found = [],
    createStatus = 201,
    syncQueued = true,
    syncStatus = 200,
  } = options;
  fetchMock.mockImplementation(
    (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const path = new URL(url, "http://localhost").pathname;
      const method = (
        input instanceof Request ? input.method : (init?.method ?? "GET")
      ).toUpperCase();
      const answer = (body: unknown, status = 200) =>
        Promise.resolve(
          new Response(JSON.stringify(body), {
            status,
            headers: { "Content-Type": "application/json" },
          }),
        );

      if (path === "/api/v1/accounts") return answer(ACCOUNTS);
      if (path.endsWith("/unparsed"))
        return answer({ operations: [], has_more: false });
      if (path === "/api/v1/instruments" && method === "GET")
        return answer({ instruments: found, has_more: false });
      if (path === "/api/v1/instruments" && method === "POST")
        return answer(
          createStatus === 201
            ? {
                id: "instr-new",
                type: "etf",
                name: "Заблокированные активы Тинькофф Технологии",
                ticker: "TECH2",
                isin: "RU000A1071G8",
                figi: "",
                currency: "RUB",
                frozen: false,
              }
            : { error: "isin already belongs to another instrument" },
          createStatus,
        );
      if (path.endsWith("/sync") && method === "POST")
        return answer(
          syncStatus === 200 ? { queued: syncQueued } : { error: "not active" },
          syncStatus,
        );
      return answer(null, 404);
    },
  );
}

// Обращения, ушедшие методом `method` на путь `path`. Отдельно от их тел:
// у запроса синхронизации тела нет вовсе, и разбор пустой строки как JSON
// уронил бы тест по причине, к предмету проверки отношения не имеющей.
function callsTo(method: string, path: string) {
  return fetchMock.mock.calls.filter(([input, init]) => {
    const url = input instanceof Request ? input.url : String(input);
    const sent = (
      input instanceof Request
        ? input.method
        : ((init as RequestInit | undefined)?.method ?? "GET")
    ).toUpperCase();
    return (
      sent === method.toUpperCase() &&
      new URL(url, "http://localhost").pathname === path
    );
  });
}

// Тела, ушедшие методом `method` на путь `path`, по порядку — то, что экран
// ПОПРОСИЛ у сервера, а не то, что нарисовал после.
async function bodiesSentTo(
  method: string,
  path: string,
): Promise<Record<string, unknown>[]> {
  const calls = callsTo(method, path);
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

describe("ReconcilePanel — бумага, которой нет в каталоге, названа паспортом брокера", () => {
  it("печатает имя, ISIN, тип и валюту, которые назвал сам брокер", async () => {
    serveCatalog({});
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [TECH2] })]);

    // Тикер брокера сам по себе — «TECH2», и он не говорит НИЧЕГО о том, что
    // это за бумага: ровно поэтому паспорт и печатается рядом.
    expect(await screen.findByText("TECH2")).toBeInTheDocument();
    const passport = await screen.findByText(/RU000A1071G8/);
    expect(passport).toHaveTextContent(
      "Заблокированные активы Тинькофф Технологии",
    );
    expect(passport).toHaveTextContent("фонд");
    expect(passport).toHaveTextContent("RUB");
  });

  // Прогон, записанный до того, как сервер стал публиковать паспорт: ключей в
  // его jsonb нет вовсе. Экран обязан не нарисовать НИЧЕГО, а не пустые скобки
  // и не «—», которые читались бы как «брокер говорит, что имени нет».
  it("ничего не выдумывает по строке старого прогона, где паспорта не было", async () => {
    serveCatalog({});
    const old: TinvestReconcileMismatch = {
      kind: "unknown_security",
      instrument_id: null,
      label: "TSPX2",
      broker: "27200",
      journal: "0",
    };
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [old] })]);

    expect(await screen.findByText("TSPX2")).toBeInTheDocument();
    expect(screen.queryByText(/Брокер о ней/)).toBeNull();
    // И кнопки нет: заводить не из чего, а сопоставляется бумага только по
    // ISIN — строка без него не закрыла бы расхождение.
    expect(
      screen.queryByRole("button", { name: /Завести в каталог/ }),
    ).toBeNull();
  });

  // Паспорт брокер не отдал (404 — так у него отвечают «забытые» бумаги), но
  // ТИП всё равно известен: он взят из самой позиции. Кнопки быть не должно —
  // без ISIN заведённая строка ни с чем не спарится.
  it("не предлагает завести бумагу, когда брокер не назвал её ISIN", async () => {
    serveCatalog({});
    const noPassport: TinvestReconcileMismatch = {
      kind: "unknown_security",
      instrument_id: null,
      label: "uid-forgotten",
      broker: "3",
      journal: "0",
      broker_type: "share",
    };
    renderPanel([
      makeReconcile({ status: "mismatched", mismatches: [noPassport] }),
    ]);

    expect(await screen.findByText("uid-forgotten")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Завести в каталог/ }),
    ).toBeNull();
  });

  it("заводит строку каталога ровно из полей паспорта и просит новую синхронизацию", async () => {
    serveCatalog({ syncQueued: true });
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [TECH2] })]);

    fireEvent.click(
      await screen.findByRole("button", { name: /Завести в каталог/ }),
    );

    await waitFor(async () => {
      expect(await bodiesSentTo("POST", "/api/v1/instruments")).toHaveLength(1);
    });
    const [body] = await bodiesSentTo("POST", "/api/v1/instruments");
    expect(body).toMatchObject({
      type: "etf",
      name: "Заблокированные активы Тинькофф Технологии",
      ticker: "TECH2",
      isin: "RU000A1071G8",
      currency: "RUB",
    });

    // Заведение само по себе расхождение не закрывает: спарить позицию с новой
    // строкой может только следующая сверка, и подпись обещает ровно это.
    expect(
      await screen.findByText(/Синхронизация поставлена в очередь/),
    ).toBeInTheDocument();
    expect(
      callsTo("POST", "/api/v1/tinvest/connections/conn-1/sync"),
    ).toHaveLength(1);
  });

  it("говорит, что бумага уже в каталоге, и ничего не заводит", async () => {
    serveCatalog({
      found: [
        {
          id: "instr-1",
          type: "etf",
          name: "Технологии Америки (заблокированные)",
          ticker: "TECH2",
          isin: "RU000A1071G8",
          figi: "",
          currency: "RUB",
          frozen: false,
        },
      ],
    });
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [TECH2] })]);

    fireEvent.click(
      await screen.findByRole("button", { name: /Завести в каталог/ }),
    );

    expect(
      await screen.findByText(
        /уже есть в каталоге: Технологии Америки \(заблокированные\)/,
      ),
    ).toBeInTheDocument();
    expect(await bodiesSentTo("POST", "/api/v1/instruments")).toHaveLength(0);
    // И синхронизацию не просим: строка была там и до нажатия, сверка уже
    // ходила по ней.
    expect(
      callsTo("POST", "/api/v1/tinvest/connections/conn-1/sync"),
    ).toHaveLength(0);
  });

  // Поиск по ISIN совпадает по ПОДСТРОКЕ имени, тикера или ISIN, поэтому чужая
  // бумага в ответе не должна сойти за эту: иначе кнопка молча сказала бы «уже
  // в каталоге» и не завела ничего.
  it("не принимает чужую бумагу из ответа поиска за эту", async () => {
    serveCatalog({
      found: [
        {
          id: "instr-2",
          type: "etf",
          name: "Другой фонд, у которого в имени RU000A1071G8",
          ticker: "OTHER",
          isin: "RU000A0000A0",
          figi: "",
          currency: "RUB",
          frozen: false,
        },
      ],
    });
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [TECH2] })]);

    fireEvent.click(
      await screen.findByRole("button", { name: /Завести в каталог/ }),
    );

    await waitFor(async () => {
      expect(await bodiesSentTo("POST", "/api/v1/instruments")).toHaveLength(1);
    });
    expect(screen.queryByText(/уже есть в каталоге/)).toBeNull();
  });

  it("не обещает синхронизацию, которую сервер отказался ставить", async () => {
    serveCatalog({ syncStatus: 409 });
    renderPanel([makeReconcile({ status: "mismatched", mismatches: [TECH2] })]);

    fireEvent.click(
      await screen.findByRole("button", { name: /Завести в каталог/ }),
    );

    expect(
      await screen.findByText(/запустить синхронизацию сейчас не удалось/),
    ).toBeInTheDocument();
  });
  it("спрашивает про дробление, когда количество отличается ровно в целое число раз", async () => {
    // Живой случай владельца: AMZN — 1 против 20 у брокера, дробление Amazon
    // 20:1 июня 2022 года, которого никто не записал. Брокер о корпоративных
    // действиях не сообщает вовсе, поэтому единственный способ, которым это
    // становится вопросом, — заметить форму расхождения.
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [
          {
            kind: "instrument",
            label: "AMZN",
            broker: "20",
            journal: "1",
            instrument_id: "instr-amzn",
            split_hint_factor: 20,
          },
        ],
      }),
    ]);

    const hint = await screen.findByTestId("mismatch-split-hint");
    expect(hint).toHaveTextContent("в 20 раз(а)");
    // ПОДСКАЗКА, А НЕ ДЕЙСТВИЕ: записать событие — решение, и принимается оно
    // в каталоге, а не кнопкой рядом с расхождением.
    expect(hint.querySelector("button")).toBeNull();
  });

  it("молчит о дроблении там, где сервер его не предположил", async () => {
    renderPanel([
      makeReconcile({
        status: "mismatched",
        mismatches: [
          {
            kind: "instrument",
            label: "FXUS",
            broker: "8830",
            journal: "8820",
            instrument_id: "instr-fxus",
          },
        ],
      }),
    ]);

    expect(await screen.findByText("FXUS")).toBeInTheDocument();
    expect(screen.queryByTestId("mismatch-split-hint")).toBeNull();
  });
});
