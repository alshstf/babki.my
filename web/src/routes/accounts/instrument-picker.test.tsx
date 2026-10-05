import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import "@/i18n";
import { InstrumentPicker } from "./instrument-picker";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// One catalog page as the endpoint answers (#104), so every test states
// whether anything is behind it.
function catalog(instruments: unknown[], hasMore = false) {
  return { instruments, has_more: hasMore };
}

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

// #88: a failed search rendered «Ничего не найдено», and the reader then
// creates a duplicate this application can neither merge nor delete.
describe("InstrumentPicker — a search that did not answer", () => {
  it("does not call a failed search «ничего не найдено»", async () => {
    serve(500, { error: "internal error" });
    renderPicker();

    expect(await screen.findByText(/не удалось получить список инструментов/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
  });

  it("still calls an empty answer «ничего не найдено»", async () => {
    serve(200, catalog([]));
    renderPicker();

    expect(await screen.findByText("Ничего не найдено")).toBeInTheDocument();
    expect(screen.queryByText(/не удалось получить список инструментов/i)).not.toBeInTheDocument();
  });

  // These start from an answer already on screen: keepPreviousData carries
  // the previous key's rows, so `data` is never empty after the first
  // search.
  it("does not carry an empty answer over to a request the browser never sent", async () => {
    serve(200, catalog([]));
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
    serve(200, catalog([]));
    renderPicker();
    expect(await screen.findByText("Ничего не найдено")).toBeInTheDocument();

    // The refined search never answers, keeping the keystroke-to-answer
    // window open.
    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "SBERBANK" },
    });

    expect(await screen.findByText(/загрузка/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
  });

  it("keeps the rows of the previous search on screen while the next one is in flight", async () => {
    // Carried-over rows are not a verdict, but they are real instruments, so
    // they stay pickable.
    serve(
      200,
      catalog([
        {
          id: "11111111-1111-1111-1111-111111111111",
          type: "share",
          name: "Сбербанк",
          ticker: "SBER",
          isin: "",
          figi: "",
          currency: "RUB",
        },
      ]),
    );
    renderPicker();
    expect(await screen.findByText("Сбербанк")).toBeInTheDocument();

    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "SBERBANK" },
    });

    expect(screen.getByText("Сбербанк")).toBeInTheDocument();
  });

  it("does not call a request the browser never sent «ничего не найдено»", async () => {
    // Offline, react-query pauses the query: status "pending", fetchStatus
    // "paused", isLoading false.
    onlineManager.setOnline(false);
    serve(200, catalog([]));
    renderPicker();

    expect(await screen.findByText(/список инструментов не загружен/i)).toBeInTheDocument();
    expect(screen.queryByText("Ничего не найдено")).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

// #104: with an empty search box this list is the catalog, and it
// stopped at page one, right above «Создать инструмент».
describe("InstrumentPicker — a catalog longer than one page", () => {
  // By offset, with different instruments per page, so appending and
  // replacing differ on screen.
  function serveByOffset(pages: { instruments: unknown[]; has_more: boolean }[]) {
    const asked: string[] = [];
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const params = new URL(url, "http://localhost").searchParams;
      asked.push(params.get("offset") ?? "");
      const index = Number(params.get("offset") ?? "0") / 2;
      return Promise.resolve(
        new Response(JSON.stringify(pages[index] ?? { instruments: [], has_more: false }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    });
    return asked;
  }

  function share(id: string, name: string) {
    return { id, type: "share", name, ticker: "", isin: "", figi: "", currency: "RUB" };
  }

  it("reaches an instrument the first page does not hold", async () => {
    const asked = serveByOffset([
      { instruments: [share("i-1", "Алроса"), share("i-2", "Банк")], has_more: true },
      { instruments: [share("i-3", "Ветер")], has_more: false },
    ]);
    renderPicker();

    expect(await screen.findByText("Алроса")).toBeInTheDocument();
    expect(screen.queryByText("Ветер")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Показать ещё" }));

    // Both pages: the second is appended.
    expect(await screen.findByText("Ветер")).toBeInTheDocument();
    expect(screen.getByText("Алроса")).toBeInTheDocument();
    // The second request starts where the first page ended.
    expect(asked).toEqual(["0", "2"]);
  });

  it("stops offering more once the server says there is none", async () => {
    serveByOffset([
      { instruments: [share("i-1", "Алроса"), share("i-2", "Банк")], has_more: true },
      { instruments: [share("i-3", "Ветер")], has_more: false },
    ]);
    renderPicker();

    fireEvent.click(await screen.findByRole("button", { name: "Показать ещё" }));

    expect(await screen.findByText("Ветер")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
  });

  it("offers nothing more when the first page is the whole catalog", async () => {
    // A full-looking page with nothing behind it: no control to fetch more.
    serveByOffset([{ instruments: [share("i-1", "Алроса"), share("i-2", "Банк")], has_more: false }]);
    renderPicker();

    expect(await screen.findByText("Алроса")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
  });

  it("does not offer to page through a search nobody is running any more", async () => {
    // Carried-over rows bring the old query's has_more. The picker has no
    // guard: react-query reports no next page for previous-query rows. Pinned
    // because that is the library's property, not this file's.
    serveByOffset([
      { instruments: [share("i-1", "Алроса"), share("i-2", "Банк")], has_more: true },
    ]);
    renderPicker();
    expect(await screen.findByText("Алроса")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Показать ещё" })).toBeInTheDocument();

    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    fireEvent.change(screen.getByPlaceholderText("Поиск инструмента"), {
      target: { value: "Ветер" },
    });

    expect(screen.getByText("Алроса")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Показать ещё" })).not.toBeInTheDocument();
  });
});
