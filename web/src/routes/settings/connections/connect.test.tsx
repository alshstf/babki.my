import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  useParams,
} from "@tanstack/react-router";
import "@/i18n";
import { ConnectWizardPage } from "./connect";
import type { SessionInfo } from "@/api/session";
import type { TinvestBrokerAccount } from "@/api/connections";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Method-aware: the create POST and the settings list GET share a path. A
// fresh Response per call, since a body can be read only once.
function serve(
  routes: { path: string; method?: string; status?: number; body?: unknown }[],
) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const method = (input instanceof Request ? input.method : init?.method) ?? "GET";
    const path = new URL(url, "http://localhost").pathname;
    const match = routes.find(
      (r) => path.endsWith(r.path) && (r.method ?? "GET").toUpperCase() === method.toUpperCase(),
    );
    return Promise.resolve(
      new Response(JSON.stringify(match?.body ?? null), {
        status: match ? (match.status ?? 200) : 404,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// The bodies POSTed to `path`, in order, read off a clone of the Request.
async function postBodies(path: string): Promise<Record<string, unknown>[]> {
  const calls = fetchMock.mock.calls.filter(([input, init]) => {
    const url = input instanceof Request ? input.url : String(input);
    const method =
      (input instanceof Request ? input.method : (init as RequestInit | undefined)?.method) ??
      "GET";
    return url.endsWith(path) && method.toUpperCase() === "POST";
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

function makeSession(overrides: Partial<SessionInfo> = {}): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Alex" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    full_valuation: "nav_and_foreign",
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

const BROKER_ACCOUNTS: TinvestBrokerAccount[] = [
  {
    broker_account_id: "b-1",
    name: "Брокерский счёт",
    type: "ACCOUNT_TYPE_TINKOFF",
    opened_on: "2020-01-01",
  },
  { broker_account_id: "b-2", name: "ИИС", type: "ACCOUNT_TYPE_TINKOFF_IIS", opened_on: null },
];

// Stands in for the connection screen: prints its id, so a test sees the
// wizard navigated to the right one. A named function, since the hooks
// lint rule goes by the binding's name.
function DetailStub() {
  const { connectionId } = useParams({ from: "/app/settings/connections/$connectionId" });
  return <div>DETAIL:{connectionId}</div>;
}

// As the router renders it, with /settings and the created connection's
// screen as stubs.
function renderWizard(session: SessionInfo = makeSession()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  // A pathless "app" layout, as in router.tsx: useParams is typed against
  // the production router.
  const layoutRoute = createRoute({
    getParentRoute: () => rootRoute,
    id: "app",
    component: () => <Outlet />,
  });
  const wizardRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings/connections/new",
    component: ConnectWizardPage,
  });
  const settingsRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings",
    component: () => <div>SETTINGS</div>,
  });
  const detailRoute = createRoute({
    getParentRoute: () => layoutRoute,
    path: "/settings/connections/$connectionId",
    component: DetailStub,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      layoutRoute.addChildren([wizardRoute, settingsRoute, detailRoute]),
    ]),
    history: createMemoryHistory({ initialEntries: ["/settings/connections/new"] }),
  });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

async function goToTokenStep() {
  fireEvent.click(await screen.findByRole("button", { name: "Далее" }));
}

async function goToAccountsStep(token = "abc123") {
  await goToTokenStep();
  fireEvent.change(await screen.findByLabelText("Токен"), { target: { value: token } });
  fireEvent.click(screen.getByRole("button", { name: "Проверить токен" }));
  await screen.findByText("Брокерский счёт");
}

beforeEach(() => {
  fetchMock.mockReset();
});

describe("ConnectWizardPage — owner gate", () => {
  it("shows the owner-only notice for a non-owner, with no wizard", async () => {
    renderWizard(makeSession({ role: "editor" }));

    expect(
      await screen.findByText("Настройки доступны только владельцу пространства"),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Далее" })).not.toBeInTheDocument();
  });
});

describe("ConnectWizardPage — the happy path", () => {
  it("checks the token, then creates the connection with exactly what was picked, then lands on its screen", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 200,
        body: { accounts: BROKER_ACCOUNTS },
      },
      {
        path: "/api/v1/tinvest/connections",
        method: "POST",
        status: 201,
        body: { id: "conn-1", status: "active", token_last4: "3456", accounts: [] },
      },
    ]);

    renderWizard();
    await goToAccountsStep("secret-token-123");

    // Only the first account is picked; its broker-given name is edited.
    fireEvent.click(screen.getByRole("checkbox", { name: "Брокерский счёт" }));
    const nameField = screen.getByDisplayValue("Брокерский счёт");
    fireEvent.change(nameField, { target: { value: "Мой брокерский" } });

    fireEvent.click(screen.getByRole("button", { name: "Подключить" }));

    // The token is checked once; the create carries the same token and only
    // the checked account, under the typed name.
    expect(await postBodies("/api/v1/tinvest/token-check")).toEqual([
      { token: "secret-token-123" },
    ]);
    expect(await postBodies("/api/v1/tinvest/connections")).toEqual([
      {
        token: "secret-token-123",
        accounts: [{ broker_account_id: "b-1", account_name: "Мой брокерский" }],
      },
    ]);

    // Landed on the freshly created connection's own screen.
    expect(await screen.findByText("DETAIL:conn-1")).toBeInTheDocument();
  });
});

describe("ConnectWizardPage — token check errors", () => {
  it("names the broker's own refusal for a 400, not the reachability message", async () => {
    serve([
      { path: "/api/v1/tinvest/token-check", method: "POST", status: 400, body: { error: "bad" } },
    ]);

    renderWizard();
    await goToTokenStep();
    fireEvent.change(await screen.findByLabelText("Токен"), { target: { value: "nope" } });
    fireEvent.click(screen.getByRole("button", { name: "Проверить токен" }));

    expect(
      await screen.findByText(
        "Брокер не принял токен. Проверьте, что он скопирован целиком, не просрочен и выпущен с доступом на чтение",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Не удалось связаться с Т-Инвестициями. Попробуйте ещё раз чуть позже"),
    ).not.toBeInTheDocument();
  });

  it("names the reachability failure for a 502, not the broker-refusal message", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 502,
        body: { error: "gateway timeout" },
      },
    ]);

    renderWizard();
    await goToTokenStep();
    fireEvent.change(await screen.findByLabelText("Токен"), { target: { value: "whatever" } });
    fireEvent.click(screen.getByRole("button", { name: "Проверить токен" }));

    expect(
      await screen.findByText(
        "Не удалось связаться с Т-Инвестициями. Попробуйте ещё раз чуть позже",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Брокер не принял токен. Проверьте, что он скопирован целиком, не просрочен и выпущен с доступом на чтение",
      ),
    ).not.toBeInTheDocument();
  });
});

describe("ConnectWizardPage — an answer that arrives late", () => {
  it("does not move the wizard forward once the owner has gone back", async () => {
    // The check is held open; «Назад» stays enabled, so leaving is invited.
    let answer: (r: Response) => void = () => {};
    fetchMock.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          answer = resolve;
        }),
    );

    renderWizard();
    await goToTokenStep();
    fireEvent.change(await screen.findByLabelText("Токен"), { target: { value: "abc123" } });
    fireEvent.click(screen.getByRole("button", { name: "Проверить токен" }));

    fireEvent.click(screen.getByRole("button", { name: "Назад" }));
    expect(await screen.findByText("Шаг 1 из 3. Выпустите токен у брокера")).toBeInTheDocument();

    await act(async () => {
      answer(
        new Response(JSON.stringify({ accounts: BROKER_ACCOUNTS }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    });

    // A late answer must not walk the wizard forward.
    expect(screen.getByText("Шаг 1 из 3. Выпустите токен у брокера")).toBeInTheDocument();
    expect(screen.queryByText("Шаг 3 из 3. Выберите счета для импорта")).not.toBeInTheDocument();

    // Forward is the next step, not the accounts the late answer carried.
    fireEvent.click(screen.getByRole("button", { name: "Далее" }));
    expect(await screen.findByText("Шаг 2 из 3. Вставьте токен")).toBeInTheDocument();
    expect(screen.queryByText("Шаг 3 из 3. Выберите счета для импорта")).not.toBeInTheDocument();
  });
});

describe("ConnectWizardPage — the accounts step", () => {
  it("keeps Подключить disabled until at least one account is checked", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 200,
        body: { accounts: BROKER_ACCOUNTS },
      },
    ]);

    renderWizard();
    await goToAccountsStep();

    expect(screen.getByRole("button", { name: "Подключить" })).toBeDisabled();

    fireEvent.click(screen.getByRole("checkbox", { name: "ИИС" }));
    expect(screen.getByRole("button", { name: "Подключить" })).toBeEnabled();

    fireEvent.click(screen.getByRole("checkbox", { name: "ИИС" }));
    expect(screen.getByRole("button", { name: "Подключить" })).toBeDisabled();
  });

  it("says the token works but has nothing to import when the broker lists no accounts", async () => {
    serve([
      { path: "/api/v1/tinvest/token-check", method: "POST", status: 200, body: { accounts: [] } },
    ]);

    renderWizard();
    await goToTokenStep();
    fireEvent.change(await screen.findByLabelText("Токен"), { target: { value: "abc123" } });
    fireEvent.click(screen.getByRole("button", { name: "Проверить токен" }));

    expect(
      await screen.findByText("Этот токен не видит ни одного счёта, который можно импортировать"),
    ).toBeInTheDocument();
  });

  // The create's two refusals, checked together: each asserts the other
  // sentence is absent.
  it("names the changed account list for a 422, and does not blame the token", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 200,
        body: { accounts: BROKER_ACCOUNTS },
      },
      {
        path: "/api/v1/tinvest/connections",
        method: "POST",
        status: 422,
        body: { error: "the token does not see that broker account" },
      },
    ]);

    renderWizard();
    await goToAccountsStep();
    fireEvent.click(screen.getByRole("checkbox", { name: "Брокерский счёт" }));
    fireEvent.click(screen.getByRole("button", { name: "Подключить" }));

    expect(
      await screen.findByText(
        "Список счетов у брокера изменился: выбранного счёта в нём больше нет. " +
          "Вернитесь на шаг назад, проверьте токен заново и выберите счета из свежего списка",
      ),
    ).toBeInTheDocument();
    // The broker just accepted this token, so re-issuing it fixes nothing.
    expect(
      screen.queryByText(
        "Брокер не принял токен. Проверьте, что он скопирован целиком, не просрочен и выпущен с доступом на чтение",
      ),
    ).not.toBeInTheDocument();
  });

  it("still blames the token for a 400 from create, and not the account list", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 200,
        body: { accounts: BROKER_ACCOUNTS },
      },
      {
        path: "/api/v1/tinvest/connections",
        method: "POST",
        status: 400,
        body: { error: "the broker refused this token" },
      },
    ]);

    renderWizard();
    await goToAccountsStep();
    fireEvent.click(screen.getByRole("checkbox", { name: "Брокерский счёт" }));
    fireEvent.click(screen.getByRole("button", { name: "Подключить" }));

    expect(
      await screen.findByText(
        "Брокер не принял токен. Проверьте, что он скопирован целиком, не просрочен и выпущен с доступом на чтение",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/Список счетов у брокера изменился/),
    ).not.toBeInTheDocument();
  });

  it("names a broker account already claimed by another connection on a 409 from create", async () => {
    serve([
      {
        path: "/api/v1/tinvest/token-check",
        method: "POST",
        status: 200,
        body: { accounts: BROKER_ACCOUNTS },
      },
      {
        path: "/api/v1/tinvest/connections",
        method: "POST",
        status: 409,
        body: { error: "already imported" },
      },
    ]);

    renderWizard();
    await goToAccountsStep();
    fireEvent.click(screen.getByRole("checkbox", { name: "Брокерский счёт" }));
    fireEvent.click(screen.getByRole("button", { name: "Подключить" }));

    expect(
      await screen.findByText("Один из выбранных счетов уже подключён другим подключением"),
    ).toBeInTheDocument();
  });
});
