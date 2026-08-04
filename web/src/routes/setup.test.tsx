import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
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
import { SetupPage } from "./setup";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a single one handed to mockResolvedValue works
// once and then throws, because a body can only be consumed once.
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
  // The router mounts its matched route asynchronously, so the form is not in
  // the document on the render call's own tick.
  fireEvent.change(await screen.findByLabelText("Название пространства"), {
    target: { value: "Наша семья" },
  });
  fireEvent.change(screen.getByLabelText("Ваше имя"), { target: { value: "Александр" } });
  fireEvent.change(screen.getByLabelText("Логин"), { target: { value: "alex" } });
  fireEvent.change(screen.getByLabelText("Пароль"), { target: { value: "12345678" } });
  fireEvent.click(screen.getByRole("button", { name: "Создать" }));
}

afterEach(() => {
  fetchMock.mockReset();
});

// The sentence that tells the reader to reload and sign in instead of filling
// this form again used to be chosen by looking for an English phrase inside the
// server's error text. The status is what the API promises (409 on
// POST /api/v1/setup, see api/openapi.yaml); the prose is not.
describe("SetupPage — which refusal it recognises", () => {
  it("recognises an instance that is already set up", async () => {
    // The refusal the server sends today, REWORDED: a client that recognises
    // it by its prose passes this only by accident, and stops recognising it
    // the day the sentence is edited.
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
