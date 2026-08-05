import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
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
import { ConnectionDetailPage } from "./detail";
import type { SessionInfo } from "@/api/session";
import type { TinvestConnection } from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

type Route = { path: string; method?: string; status?: number; body?: unknown };

// Method-aware, because this screen sends a GET, a PATCH, a POST and a DELETE
// at paths that overlap. A fresh Response per matched route per call —
// mockResolvedValue's single object breaks on a second call, since a body can
// only be read once.
function serve(routes: Route[]) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const method = (input instanceof Request ? input.method : init?.method) ?? "GET";
    const path = new URL(url, "http://localhost").pathname;
    const match = routes.find(
      (r) => path.endsWith(r.path) && (r.method ?? "GET").toUpperCase() === method.toUpperCase(),
    );
    const status = match ? (match.status ?? 200) : 404;
    // A 204 carries no body at all — constructing one with a body throws.
    if (status === 204) return Promise.resolve(new Response(null, { status }));
    return Promise.resolve(
      new Response(JSON.stringify(match?.body ?? null), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// The bodies actually sent to `path` by `method`, in order — what the screen
// asked the server for, not what it drew afterwards. openapi-fetch hands
// globalThis.fetch one Request, so the body is read off a clone.
async function bodiesSent(path: string, method: string): Promise<Record<string, unknown>[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const url = input instanceof Request ? input.url : String(input);
    const sent =
      (input instanceof Request ? input.method : (init as RequestInit | undefined)?.method) ??
      "GET";
    return url.endsWith(path) && sent.toUpperCase() === method.toUpperCase();
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

// Requests to a path that carries a query string — the run log is fetched with
// limit and offset on it, so `endsWith` on the whole URL would never match.
function requestsTo(pathSuffix: string): number {
  return fetchMock.mock.calls.filter(([input]) => {
    const url = new URL(input instanceof Request ? input.url : String(input), "http://localhost");
    return url.pathname.endsWith(pathSuffix);
  }).length;
}

function callCount(path: string, method: string): number {
  return fetchMock.mock.calls.filter(([input, init]) => {
    const url = input instanceof Request ? input.url : String(input);
    const sent =
      (input instanceof Request ? input.method : (init as RequestInit | undefined)?.method) ??
      "GET";
    return url.endsWith(path) && sent.toUpperCase() === method.toUpperCase();
  }).length;
}

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
    ...overrides,
  };
}

function makeConnection(overrides: Partial<TinvestConnection> = {}): TinvestConnection {
  return {
    id: "conn-1",
    status: "active",
    token_last4: "3456",
    accounts: [
      {
        link_id: "link-1",
        account_id: "acc-1",
        broker_account_id: "b-1",
        broker_account_name: "Брокерский счёт",
        broker_account_type: "ACCOUNT_TYPE_TINKOFF",
        opened_on: "2020-01-01",
      },
    ],
    last_successful_sync_at: "2026-08-04T09:15:00Z",
    // One verdict per linked account, the shape the server always sends: an
    // account nothing ever checked is present here saying so.
    reconciles: [
      {
        link_id: "link-1",
        account_id: "acc-1",
        broker_account_name: "Брокерский счёт",
        at: null,
        status: "not_checked",
        mismatches: [],
      },
    ],
    ...overrides,
  };
}

// Everything the screen fetches beyond the connection itself, answered empty,
// so a test about the header is not also a test of the three panels below it.
function quietBackground(): Route[] {
  return [
    { path: "/api/v1/accounts", body: [{ id: "acc-1", name: "Т-Инвестиции: брокерский" }] },
    {
      path: "/api/v1/tinvest/connections/conn-1/runs",
      body: { runs: [], has_more: false },
    },
    {
      path: "/api/v1/tinvest/connections/conn-1/unparsed",
      body: { operations: [], has_more: false },
    },
  ];
}

// `extra` goes first: the first matching route answers, so a test that wants
// to say something about the run log or the unparsed list overrides the quiet
// defaults rather than being shadowed by them.
function serveConnection(connection: TinvestConnection, extra: Route[] = []) {
  serve([
    ...extra,
    { path: "/api/v1/tinvest/connections/conn-1", body: connection },
    ...quietBackground(),
  ]);
}

// Renders the screen the way router.tsx does, nested under a pathless "app"
// layout with that same id: useParams({ from: ... }) is type-checked against
// the PRODUCTION router, so the id has to read
// "/app/settings/connections/$connectionId" whatever this tree looks like. The
// two places the screen can send the owner — /settings after a delete, an
// account behind a link — are stubs that only say where they are.
function renderPage(session: SessionInfo = makeSession()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => <Outlet />,
  });
  const detailRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings/connections/$connectionId",
    component: ConnectionDetailPage,
  });
  const settingsRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings",
    component: () => <div>SETTINGS</div>,
  });
  const accountRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/accounts/$accountId",
    component: () => <div>ACCOUNT</div>,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      layoutRoute.addChildren([detailRoute, settingsRoute, accountRoute]),
    ]),
    history: createMemoryHistory({ initialEntries: ["/settings/connections/conn-1"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

// The card that lists the linked accounts. The screen names an account in two
// places — here, and beside that account's own reconcile verdict — so an
// assertion about this card is scoped to it rather than to the whole page.
async function accountsCard(): Promise<HTMLElement> {
  const title = await screen.findByText("Связанные счета");
  const card = title.closest("[data-slot=card]");
  if (!card) throw new Error("the linked-accounts card is not a card");
  return card as HTMLElement;
}

beforeEach(() => {
  fetchMock.mockReset();
  serveConnection(makeConnection());
});

describe("ConnectionDetailPage — who may see it", () => {
  it("shows the owner-only notice for a non-owner, and asks the server for nothing", async () => {
    renderPage(makeSession({ role: "editor" }));

    expect(
      await screen.findByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
    expect(callCount("/api/v1/tinvest/connections/conn-1", "GET")).toBe(0);
  });

  it("says a missing connection is missing rather than reporting a fault", async () => {
    serve([
      { path: "/api/v1/tinvest/connections/conn-1", status: 404, body: { error: "not found" } },
      ...quietBackground(),
    ]);
    renderPage();

    expect(
      await screen.findByText("Такого подключения нет — возможно, его уже удалили"),
    ).toBeInTheDocument();
  });
});

describe("ConnectionDetailPage — the header", () => {
  it("says when the last successful sync STARTED, and shows the token's tail", async () => {
    renderPage();

    expect(
      await screen.findByText("Последняя удачная синхронизация началась 04.08.2026, 12:15"),
    ).toBeInTheDocument();
    expect(screen.getByText("Токен ···3456")).toBeInTheDocument();
    expect(screen.getByText("Активно")).toBeInTheDocument();
  });

  // The field is keyed by the connection while runs are made per account, so
  // for several accounts it means at least one of them synced then.
  it("does not claim every account synced when the connection feeds more than one", async () => {
    serveConnection(
      makeConnection({
        accounts: [
          ...makeConnection().accounts,
          {
            link_id: "link-2",
            account_id: "acc-2",
            broker_account_id: "b-2",
            broker_account_name: "ИИС",
            broker_account_type: "ACCOUNT_TYPE_TINKOFF_IIS",
            opened_on: null,
          },
        ],
      }),
    );
    renderPage();

    expect(
      await screen.findByText(
        "Последняя удачная синхронизация началась 04.08.2026, 12:15 — хотя бы по одному из счетов",
      ),
    ).toBeInTheDocument();
  });

  it("says outright that there has never been a successful sync", async () => {
    serveConnection(makeConnection({ last_successful_sync_at: null }));
    renderPage();

    expect(await screen.findByText("Удачных синхронизаций ещё не было")).toBeInTheDocument();
  });

  // Deleting a babki account takes its link with it and leaves the connection
  // standing, so a connection with no accounts left is reachable.
  it("says a connection has nowhere left to import into", async () => {
    serveConnection(makeConnection({ accounts: [] }));
    renderPage();

    expect(
      await screen.findByText(
        "Связанных счетов не осталось: этому подключению больше некуда загружать операции",
      ),
    ).toBeInTheDocument();
  });

  it("names both ends of a link without borrowing one name for the other", async () => {
    renderPage();

    const card = await accountsCard();
    expect(within(card).getByRole("link", { name: "Т-Инвестиции: брокерский" })).toHaveAttribute(
      "href",
      "/accounts/acc-1",
    );
    expect(within(card).getByText("У брокера: Брокерский счёт")).toBeInTheDocument();
    expect(
      within(card).getByText("Тип счёта у брокера: ACCOUNT_TYPE_TINKOFF"),
    ).toBeInTheDocument();
  });

  // Both labels are written when the link is made and neither is re-read on a
  // sync, so the warning belongs to both. It used to be on the name alone,
  // which left the type reading as what the broker calls it today.
  it("says of the type, as of the name, that it is what the broker said then", async () => {
    renderPage();

    const card = await accountsCard();
    expect(within(card).getByText("У брокера: Брокерский счёт")).toHaveAttribute(
      "title",
      "Так брокер называл счёт в момент подключения. С тех пор счёт мог быть переименован",
    );
    expect(within(card).getByText("Тип счёта у брокера: ACCOUNT_TYPE_TINKOFF")).toHaveAttribute(
      "title",
      "Так брокер классифицировал счёт в момент подключения. Эта запись больше не обновляется: если тип у брокера изменится, здесь останется прежний",
    );
  });
});

describe("ConnectionDetailPage — a token the broker refused", () => {
  it("says what happened, offers a new token, and offers no switch that would not fix it", async () => {
    serveConnection(makeConnection({ status: "token_revoked" }));
    renderPage();

    expect(
      await screen.findByText(
        "Брокер отказался принимать сохранённый токен. Новые операции не загружаются, пока вы не вставите новый",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Вставить новый токен" })).toBeInTheDocument();
    // Neither switch: «включить» would set active on a token the broker has
    // already refused, and the next run would park it right back.
    expect(screen.queryByRole("button", { name: "Включить" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Выключить" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Синхронизировать сейчас" })).toBeDisabled();
  });

  it("sends the pasted token and reports that the broker took it", async () => {
    serveConnection(makeConnection({ status: "token_revoked" }), [
      {
        path: "/api/v1/tinvest/connections/conn-1",
        method: "PATCH",
        body: makeConnection({ status: "active" }),
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Вставить новый токен" }));
    fireEvent.change(screen.getByLabelText("Новый токен"), {
      target: { value: "fresh-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить токен" }));

    expect(await screen.findByText("Брокер принял новый токен")).toBeInTheDocument();
    expect(await bodiesSent("/api/v1/tinvest/connections/conn-1", "PATCH")).toEqual([
      { token: "fresh-token" },
    ]);
  });

  it("says the broker refused a token, by status and not by the server's words", async () => {
    serveConnection(makeConnection({ status: "token_revoked" }), [
      {
        path: "/api/v1/tinvest/connections/conn-1",
        method: "PATCH",
        status: 400,
        body: { error: "tinvest: token rejected" },
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Вставить новый токен" }));
    fireEvent.change(screen.getByLabelText("Новый токен"), { target: { value: "nope" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить токен" }));

    expect(
      await screen.findByText(
        "Брокер не принял токен. Проверьте, что он скопирован целиком, не просрочен и выпущен с доступом на чтение",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("Брокер принял новый токен")).not.toBeInTheDocument();
  });
});

describe("ConnectionDetailPage — switching the import off and on", () => {
  it("switches a working connection off", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1",
        method: "PATCH",
        body: makeConnection({ status: "disabled" }),
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Выключить" }));

    // The request itself is the observable effect here: the GET behind this
    // screen keeps answering with the connection as it was, so nothing on the
    // screen would change to wait for.
    await waitFor(async () => {
      expect(await bodiesSent("/api/v1/tinvest/connections/conn-1", "PATCH")).toEqual([
        { status: "disabled" },
      ]);
    });
  });

  it("explains a switched-off connection and does not offer to sync it", async () => {
    serveConnection(makeConnection({ status: "disabled" }));
    renderPage();

    expect(
      await screen.findByText(
        "Подключение выключено: расписание его пропускает. Уже загруженные счета и операции остаются на месте",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Включить" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Синхронизировать сейчас" })).toBeDisabled();
    // Said in text rather than in a tooltip: a disabled button carries
    // pointer-events-none, so a `title` on it is unreachable.
    expect(
      screen.getByText("Синхронизировать можно только активное подключение"),
    ).toBeInTheDocument();
  });

  it("says nothing about activity being required while the connection is active", async () => {
    serveConnection(makeConnection());
    renderPage();

    await screen.findByText("Активно");
    expect(
      screen.queryByText("Синхронизировать можно только активное подключение"),
    ).not.toBeInTheDocument();
  });
});

describe("ConnectionDetailPage — the sync button", () => {
  it("says the sync was queued when this request is what queued it", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1/sync",
        method: "POST",
        status: 202,
        body: { queued: true },
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Синхронизировать сейчас" }));

    expect(await screen.findByText("Синхронизация поставлена в очередь")).toBeInTheDocument();
  });

  // `queued: false` covers a job waiting out a failed attempt's backoff, which
  // River grows into the hours — so «уже идёт» would be false for as long as
  // that wait lasts.
  it("does not claim a sync is running when the server only said one was already queued", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1/sync",
        method: "POST",
        status: 202,
        body: { queued: false },
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Синхронизировать сейчас" }));

    expect(
      await screen.findByText(
        "Синхронизация уже в очереди: она либо идёт сейчас, либо ждёт своего часа — после неудачной попытки ожидание может длиться часами",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("Синхронизация поставлена в очередь")).not.toBeInTheDocument();
  });

  // The queued run only becomes visible in the log below, so the log is what
  // has to be asked again: without that the owner presses the button, is told
  // the sync is queued, and sees a log that stays exactly as it was until the
  // page is reloaded.
  it("asks the run log again after queueing a sync", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1/sync",
        method: "POST",
        status: 202,
        body: { queued: true },
      },
    ]);
    renderPage();

    await screen.findByText("Синхронизаций ещё не было");
    const before = requestsTo("/runs");
    fireEvent.click(screen.getByRole("button", { name: "Синхронизировать сейчас" }));

    await waitFor(() => {
      expect(requestsTo("/runs")).toBeGreaterThan(before);
    });
  });

  // «Поставлена в очередь» is about the press that produced it. It used to
  // stay on screen for the rest of the visit — including over a replaced
  // token, where it says something about a queue nobody has looked at since.
  it("stops saying a sync was queued once another action is taken", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1/sync",
        method: "POST",
        status: 202,
        body: { queued: true },
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Синхронизировать сейчас" }));
    expect(await screen.findByText("Синхронизация поставлена в очередь")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Вставить новый токен" }));

    await waitFor(() => {
      expect(screen.queryByText("Синхронизация поставлена в очередь")).not.toBeInTheDocument();
    });
  });

  it("reports a 409 as the connection no longer being active", async () => {
    serveConnection(makeConnection(), [
      {
        path: "/api/v1/tinvest/connections/conn-1/sync",
        method: "POST",
        status: 409,
        body: { error: "connection is not active" },
      },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Синхронизировать сейчас" }));

    expect(
      await screen.findByText("Синхронизация не запустилась: подключение сейчас не активно"),
    ).toBeInTheDocument();
  });
});

describe("ConnectionDetailPage — deleting the connection", () => {
  it("warns that the accounts and their operations stay, then deletes and leaves the screen", async () => {
    serveConnection(makeConnection(), [
      { path: "/api/v1/tinvest/connections/conn-1", method: "DELETE", status: 204 },
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Удалить подключение" }));

    expect(await screen.findByText("Удалить подключение к Т-Инвестициям?")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Будут удалены сохранённый токен, копия операций брокера, журнал синхронизаций и сверка. Счета в babki.my и уже загруженные в них операции останутся на месте — они просто перестанут обновляться",
      ),
    ).toBeInTheDocument();

    // The dialog's own button, not the one on the card behind it.
    const confirm = screen
      .getAllByRole("button", { name: "Удалить подключение" })
      .at(-1) as HTMLElement;
    fireEvent.click(confirm);

    expect(await screen.findByText("SETTINGS")).toBeInTheDocument();
    expect(callCount("/api/v1/tinvest/connections/conn-1", "DELETE")).toBe(1);
  });

  it("deletes nothing until the confirmation is given", async () => {
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Удалить подключение" }));
    await screen.findByText("Удалить подключение к Т-Инвестициям?");
    fireEvent.click(screen.getByRole("button", { name: "Отмена" }));

    expect(callCount("/api/v1/tinvest/connections/conn-1", "DELETE")).toBe(0);
  });
});

describe("ConnectionDetailPage — the panels below", () => {
  it("shows the reconcile verdict, the run log and the unparsed list together", async () => {
    serveConnection(
      makeConnection({
        reconciles: [
          {
            link_id: "link-1",
            account_id: "acc-1",
            broker_account_name: "Брокерский счёт",
            at: "2026-08-04T09:16:00Z",
            status: "mismatched",
            mismatches: [
              { kind: "instrument", instrument_id: "i-1", label: "SBER", broker: "150", journal: "100" },
            ],
          },
        ],
      }),
      [
        {
          path: "/api/v1/tinvest/connections/conn-1/runs",
          body: {
            runs: [
              {
                id: "run-1",
                link_id: "link-1",
                trigger: "manual",
                status: "ok",
                started_at: "2026-08-04T09:15:00Z",
                finished_at: "2026-08-04T09:16:00Z",
                read_count: 120,
                added_count: 3,
                disappeared_count: 0,
                unparsed_count: 1,
                error: "",
                reconcile_status: "mismatched",
                reconciled_at: "2026-08-04T09:16:00Z",
                mismatches: [
                  {
                    kind: "instrument",
                    instrument_id: "i-1",
                    label: "SBER",
                    broker: "150",
                    journal: "100",
                  },
                ],
              },
            ],
            has_more: false,
          },
        },
        {
          path: "/api/v1/tinvest/connections/conn-1/unparsed",
          body: {
            operations: [
              {
                id: "u-1",
                occurred_at: "2026-08-03T10:00:00Z",
                op_type: "OPERATION_TYPE_FUTURES",
                payment: "-100",
                currency: "RUB",
                description: "",
                reason: "unsupported_type",
                raw: {},
              },
            ],
            has_more: false,
          },
        },
      ],
    );
    renderPage();

    expect(await screen.findByText("Расходится с брокером")).toBeInTheDocument();
    expect(await screen.findByText("Прочитано у брокера: 120")).toBeInTheDocument();
    expect(screen.getByText("Вручную")).toBeInTheDocument();
    expect(
      await screen.findByText("Тип операции пока не поддерживается"),
    ).toBeInTheDocument();
    // The counter beside the verdict reads the very list printed below it.
    expect(screen.getByText("Неразобранных операций: 1")).toBeInTheDocument();
  });

  // THE CASE THE SCREEN USED TO GET WRONG, end to end. Two broker accounts:
  // one differs, the other agrees and was checked a moment later. A single
  // verdict for the connection was the newest of the two, so the screen drew a
  // tick and «Сходится с брокером» — while the run log two cards below showed
  // the differing account's run saying «Расхождение».
  it("shows a verdict per account and claims no agreement when one of them differs", async () => {
    serveConnection(
      makeConnection({
        accounts: [
          ...makeConnection().accounts,
          {
            link_id: "link-2",
            account_id: "acc-2",
            broker_account_id: "b-2",
            broker_account_name: "ИИС",
            broker_account_type: "ACCOUNT_TYPE_TINKOFF_IIS",
            opened_on: null,
          },
        ],
        reconciles: [
          {
            link_id: "link-1",
            account_id: "acc-1",
            broker_account_name: "Брокерский счёт",
            at: "2026-08-04T09:15:00Z",
            status: "mismatched",
            mismatches: [
              { kind: "currency", instrument_id: null, label: "RUB", broker: "1000.5", journal: "0" },
            ],
          },
          {
            link_id: "link-2",
            account_id: "acc-2",
            broker_account_name: "ИИС",
            at: "2026-08-04T09:16:00Z",
            status: "matched",
            mismatches: [],
          },
        ],
      }),
    );
    renderPage();

    // Both verdicts are on screen, each beside the account it was made for.
    expect(await screen.findByText("Расходится с брокером")).toBeInTheDocument();
    expect(screen.getByText("Сходится с брокером")).toBeInTheDocument();
    expect(screen.getAllByText("У брокера: Брокерский счёт").length).toBeGreaterThan(0);
    expect(screen.getAllByText("У брокера: ИИС").length).toBeGreaterThan(0);
    // The differing account's own figures, which the old screen never reached.
    expect(screen.getByText("1000.5")).toBeInTheDocument();
    // And no claim that the connection as a whole agrees.
    expect(screen.getByText("Есть расхождения с брокером")).toBeInTheDocument();
    expect(
      screen.queryByText("Все счета сверены и сходятся с брокером"),
    ).not.toBeInTheDocument();
  });
});
