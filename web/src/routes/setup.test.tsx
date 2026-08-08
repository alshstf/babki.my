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

// What a browser with no connection does: fetch rejects and no status is ever
// read — a failure that carries no answer at all. Separate from serve() above
// rather than a flag on it, so the two existing callers stay as they read.
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
  // react-query's onlineManager is a module-level singleton, so a test that
  // takes the browser offline has to put it back or every later test in the
  // run inherits it.
  onlineManager.setOnline(true);
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

// Issue #111. This is the only screen a brand-new instance can show, and it was
// the one mutation in session.ts left on react-query's default networkMode,
// "online" — which PAUSES a mutation while the browser reports itself offline.
describe("SetupPage — a browser that reports no connection", () => {
  it("says so, sends the request anyway, and lets the reader try again", async () => {
    // Held rather than failed was the whole defect: nothing went out, isError
    // stayed false so the alert had nothing to show, and isPending stayed true
    // so the button that would try again was disabled. The reader pressed
    // «Создать», saw nothing happen and nothing said — and then, whenever the
    // connection returned, the held request went out and the instance set
    // itself up on its own, an arbitrary time after anyone asked.
    onlineManager.setOnline(false);
    serveNetworkError();

    await fillAndSubmit();

    // The sentence claims nothing about the cause, which is what makes it true
    // here: «Проверьте поля», the wording until this fix, is a verdict on what
    // was typed, and the fields are perfectly good on a dead connection.
    // Compared whole so that reintroducing that clause reddens this.
    expect(
      await screen.findByText("Не удалось выполнить настройку. Попробуйте ещё раз"),
    ).toBeInTheDocument();
    // Attempted, not held: the browser's own idea of being offline does not get
    // to decide this one, and it is the request that reports the answer.
    expect(fetchMock).toHaveBeenCalled();
    // And the reader can try again — a locked button is the same silence.
    expect(createButton()).not.toBeDisabled();
  });
});
