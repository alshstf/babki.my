import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import "@/i18n";
import { SetupPage } from "./setup";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
function serve(status: number, body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

// fetch rejects, as a browser with no connection does.
function serveNetworkError() {
  fetchMock.mockImplementation(() => Promise.reject(new TypeError("Failed to fetch")));
}

const createButton = () => screen.getByRole("button", { name: "Создать" });

// The page navigates on success (useSetup), which needs a router in context.
async function fillAndSubmit() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const rootRoute = createRootRoute({ component: () => <Outlet /> });
  const indexRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: SetupPage,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([indexRoute]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  // The router mounts the route asynchronously.
  fireEvent.change(await screen.findByLabelText("Название пространства"), {
    target: { value: "Наша семья" },
  });
  fireEvent.change(screen.getByLabelText("Ваше имя"), { target: { value: "Александр" } });
  fireEvent.change(screen.getByLabelText("Логин"), { target: { value: "alex" } });
  fireEvent.change(screen.getByLabelText("Пароль"), { target: { value: "12345678" } });
  fireEvent.click(screen.getByRole("button", { name: "Создать" }));
}

afterEach(() => {
  // onlineManager is a module-level singleton: put it back.
  onlineManager.setOnline(true);
  fetchMock.mockReset();
});

// The refusal is recognised by status (409 on POST /api/v1/setup), which
// the API promises, not by the prose, which it does not.
describe("SetupPage — which refusal it recognises", () => {
  it("recognises an instance that is already set up", async () => {
    // The prose reworded: recognising it by text would pass only by accident.
    serve(409, { error: "this instance has already been configured" });
    await fillAndSubmit();

    expect(await screen.findByText(/Инстанс уже настроен/)).toBeInTheDocument();
  });

  it("does not claim it is already set up when the server said something else", async () => {
    serve(400, { error: "password must be at least 8 characters" });
    await fillAndSubmit();

    expect(await screen.findByText(/Не удалось выполнить настройку/)).toBeInTheDocument();
    expect(screen.queryByText(/Инстанс уже настроен/)).toBeNull();
    expect(document.body.textContent).not.toContain("at least 8 characters");
  });
});

// #111: the only screen a new instance shows; offline, the default
// networkMode "online" would pause the mutation.
describe("SetupPage — a browser that reports no connection", () => {
  it("says so, sends the request anyway, and lets the reader try again", async () => {
    // Held, the button stayed disabled with nothing said, and the instance
    // set itself up whenever the connection returned.
    onlineManager.setOnline(false);
    serveNetworkError();

    await fillAndSubmit();

    // The sentence claims no cause: «Проверьте поля» would blame good
    // fields. Compared whole so that clause cannot come back.
    expect(
      await screen.findByText("Не удалось выполнить настройку. Попробуйте ещё раз"),
    ).toBeInTheDocument();
    // Attempted, not held.
    expect(fetchMock).toHaveBeenCalled();
    // And the reader can try again — a locked button is the same silence.
    expect(createButton()).not.toBeDisabled();
  });
});

// A running server asks for the one-time code it logged: the form sends
// it and says when it was wrong.
describe("SetupPage — the one-time code from the server's log", () => {
  it("asks for the code, sends it, and names a wrong one", async () => {
    const sent: unknown[] = [];
    fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
      const req = input as Request;
      const path = new URL(req.url, "http://localhost").pathname;
      if (path.endsWith("/api/v1/setup/status")) {
        return new Response(JSON.stringify({ setup_needed: true, code_required: true }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      sent.push(JSON.parse(await req.text()));
      return new Response(JSON.stringify({ error: "x" }), { status: 403, headers: { "Content-Type": "application/json" } });
    });
    const codeField = async () => screen.findByLabelText("Код первого запуска");
    const renderAndFill = fillAndSubmit;
    // With a code required and none typed, the button stays disabled.
    await renderAndFill();
    expect(sent).toHaveLength(0);
    fireEvent.change(await codeField(), { target: { value: " k7m2p9qx " } });
    fireEvent.click(createButton());
    expect(await screen.findByText(/Код не подошёл/)).toBeInTheDocument();
    expect(sent).toEqual([expect.objectContaining({ setup_code: "k7m2p9qx" })]);
  });
});
