import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { PaperClass } from "./paper-class";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

// AKMB as the broker files it (2026-10-13): Russia, municipal shares.
const fromBroker = {
  country: "RU", sector: "municipal", country_by_hand: false, sector_by_hand: false, source: "tinvest", raw: "municipal equity",
  fetched_at: "2026-10-13T12:00:00Z", source_country: "RU", source_sector: "municipal",
  sectors: ["financials", "municipal", "funds_bonds"],
};

function show(canEdit: boolean) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (input.method === "PUT") return json({ ...fromBroker, sector: "funds_bonds", sector_by_hand: true });
    return json(fromBroker);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <PaperClass instrumentId="i-1" canEdit={canEdit} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("PaperClass", () => {
  it("says where the country and sector come from, and takes the family's own", async () => {
    show(true);
    const line = await screen.findByTestId("instrument-class");
    expect(norm(line.textContent)).toMatch(/Страна риска: Россия · по данным Т-Инвестиций.*Отрасль: Регионы и муниципалитеты · по данным Т-Инвестиций/);
    fireEvent.click(screen.getByRole("button", { name: "Изменить" }));
    expect(await screen.findByText("У источника: «municipal equity»")).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Отрасль" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Фонды облигаций" }));
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(put.url).toMatch(/\/instruments\/i-1\/class$/);
    expect(await put.json()).toEqual({ country: null, sector: "funds_bonds" });
    await waitFor(() => expect(norm(screen.getByTestId("instrument-class").textContent)).toMatch(/Отрасль: Фонды облигаций · указано вами/));
  });

  it("gives a viewer nothing to change", async () => {
    show(false);
    await screen.findByTestId("instrument-class");
    expect(screen.queryByRole("button", { name: "Изменить" })).toBeNull();
  });
});
