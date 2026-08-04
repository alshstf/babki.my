import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import "@/i18n";
import { InstrumentPicker } from "./instrument-picker";

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

function renderPicker() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <InstrumentPicker value={null} onChange={() => {}} />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  fetchMock.mockReset();
  onlineManager.setOnline(true);
});

// #88: the search was the one query in this application with no error branch.
// A request that failed rendered the same «Ничего не найдено» as a request that
// succeeded and found nothing — and the next thing the reader does is create
// the instrument that already exists, which nothing in this application can
// then merge or delete.
describe("InstrumentPicker — a search that did not answer", () => {
  it("does not call a failed search «ничего не найдено»", async () => {
    serve(500, { error: "internal error" });
    renderPicker();

    expect(await screen.findByText(/не удалось получить список инструментов/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
  });

  it("still calls an empty answer «ничего не найдено»", async () => {
    serve(200, []);
    renderPicker();

    expect(await screen.findByText("Ничего не найдено")).toBeInTheDocument();
    expect(screen.queryByText(/не удалось получить список инструментов/i)).not.toBeInTheDocument();
  });

  it("does not call a request the browser never sent «ничего не найдено»", async () => {
    // react-query holds a query instead of sending it while the browser reports
    // itself offline (networkMode "online", the default): status stays
    // "pending" while fetchStatus is "paused", so isLoading — which is
    // isPending && isFetching — is FALSE here, and a list keyed on isLoading
    // alone falls straight through to the empty caption.
    onlineManager.setOnline(false);
    serve(200, []);
    renderPicker();

    expect(await screen.findByText(/список инструментов не загружен/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
