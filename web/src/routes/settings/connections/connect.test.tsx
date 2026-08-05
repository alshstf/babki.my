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

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Method-aware, unlike a plain path match: POST /api/v1/tinvest/connections
// and GET /api/v1/tinvest/connections (the settings list) share a path, and a
// mock keyed on the path alone would answer the wizard's create with a list
// envelope instead of a connection. A fresh Response per matched route per
// call — mockResolvedValue's single object breaks on a second call because a
// body can only be read once.
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

// The bodies actually sent to `path` by POST, in order — what the wizard
// asked the server for, not what it rendered afterwards. openapi-fetch hands
// globalThis.fetch a single Request object, so the body is read off a clone
// (it can only be consumed once).
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

// Stands in for the real connection screen (a later task's job): it only
// prints the id it was given, which is all a test here needs to tell that the
// wizard actually navigated there — with the right connection — rather than
// merely calling the mutation. A named function (not an inline arrow passed
// as `component:`) because it calls a hook, and the lint rule that checks
// hooks are called only from components or other hooks goes by the binding's
// name.
function DetailStub() {
  const { connectionId } = useParams({ from: "/app/settings/connections/$connectionId" });
  return <div>DETAIL:{connectionId}</div>;
}

// Renders the wizard the way the router does (/settings/connections/new),
// with the two screens it can go to as stand-ins: /settings (cancel target)
// and the created connection's own screen, which just prints the id it was
// given so a test can tell the wizard actually navigated there rather than
// merely calling the mutation.
function renderWizard(session: SessionInfo = makeSession()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], session);
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  // Nested under a pathless "app" id, mirroring router.tsx's own layoutRoute:
  // useParams({ from: ... }) below is type-checked against the PRODUCTION
  // router (the global Register — see router.tsx), so the id has to read
  // "/app/settings/connections/$connectionId" regardless of this test's own
  // tree, and only matches something real at runtime if that tree actually
  // has the same "app" layer (the same reason detail.test.tsx nests one for
  // the accounts screen's equivalent route).
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

    // The token went to the check exactly once, and the create carries the
    // SAME token plus only the account that was checked, under the name
    // typed for it — never the broker's own name for it, and never the
    // account that was left unchecked (ИИС).
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
    // The check is held open so the owner can leave the step it was started
    // from — the ordinary case of a slow broker, not a contrived one. The
    // «Назад» button is not disabled while the check runs, so leaving is
    // something the screen invites.
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

    // The owner is where they put themselves. A late answer that walks the
    // wizard two steps forward reads as the screen having a mind of its own.
    expect(screen.getByText("Шаг 1 из 3. Выпустите токен у брокера")).toBeInTheDocument();
    expect(screen.queryByText("Шаг 3 из 3. Выберите счета для импорта")).not.toBeInTheDocument();

    // Going forward is still going forward: the wizard walks its own steps in
    // order, and the next one is the token, not the accounts the late answer
    // happened to carry.
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

  // The two refusals create can answer that both used to be one 400. The
  // pair is checked together, and each leg asserts the OTHER sentence is
  // absent: a screen that showed both, or showed the token's sentence for
  // either, would pass a test that only looked for the one it expected.
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
    // The token in this request is the one the broker just accepted at the
    // check. Telling the owner to re-issue it sends them to fix what is not
    // broken — the whole reason the server stopped answering 400 here.
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
