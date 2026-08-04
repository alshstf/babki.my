import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
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

  // The three below start from an answer already on screen, which is what the
  // first version of this fix could not see: it went offline BEFORE the first
  // render, so there was no previous answer for the next query to inherit, and
  // a picker keyed on `data === undefined` alone passed. useInstruments hands
  // the previous key's rows to the next key (placeholderData: keepPreviousData),
  // so from the second search onwards there is always something in `data`.
  it("does not carry an empty answer over to a request the browser never sent", async () => {
    serve(200, []);
    renderPicker();
    expect(await screen.findByText("Ничего не найдено")).toBeInTheDocument();

    onlineManager.setOnline(false);
    fetchMock.mockClear();
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "SBERBANK" },
    });

    expect(await screen.findByText(/список инструментов не загружен/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("does not carry an empty answer over to a search that is still in flight", async () => {
    serve(200, []);
    renderPicker();
    expect(await screen.findByText("Ничего не найдено")).toBeInTheDocument();

    // The refined search never comes back, so the window between the keystroke
    // and the answer — milliseconds online, and enough of them to click
    // «Создать инструмент» in — stays open for the assertion.
    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "SBERBANK" },
    });

    expect(await screen.findByText(/загрузка/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
  });

  it("keeps the rows of the previous search on screen while the next one is in flight", async () => {
    // The other half of the decision: rows carried over are not a verdict, but
    // they are real instruments and picking one is right whatever query fetched
    // them, so they stay rather than flashing away on every keystroke.
    serve(200, [
      {
        id: "11111111-1111-1111-1111-111111111111",
        type: "share",
        name: "Сбербанк",
        ticker: "SBER",
        isin: "",
        figi: "",
        currency: "RUB",
      },
    ]);
    renderPicker();
    expect(await screen.findByText("Сбербанк")).toBeInTheDocument();

    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "SBERBANK" },
    });

    expect(screen.getByText("Сбербанк")).toBeInTheDocument();
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
