import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { TargetsCard } from "./targets";
import type { Allocation, AllocationRow } from "@/api/allocation";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const row = (key: string, minor: number, share: string, target?: string, gap?: string, off?: number): AllocationRow => ({
  key, minor, share, target: target ?? null, gap: gap ?? null, off_minor: off ?? null,
});

// The note's family (Р-37).
const allocation: Allocation = {
  base_currency: "RUB", mode: "topup", worth_minor: 3_000_000_00, everyday_minor: 200_000_00, unsplit_minor: 0,
  kinds: [
    row("shares", 1_050_000_00, "35.00", "30.00", "5.00", 150_000_00),
    row("bonds", 600_000_00, "20.00", "30.00", "-10.00", -300_000_00),
    row("funds", 450_000_00, "15.00", "15.00", "0.00", 0),
    row("metals", 0, "0.00", "10.00", "-10.00", -300_000_00),
    row("deposits", 600_000_00, "20.00", "15.00", "5.00", 150_000_00),
    row("broker_cash", 300_000_00, "10.00", "0.00", "10.00", 300_000_00),
  ],
  currencies: [row("RUB", 2_100_000_00, "70.00"), row("USD", 900_000_00, "30.00")],
  unpriced: 0, missing_rates: [],
};

function answer(role: string, a: Allocation) {
  fetchMock.mockImplementation((input: Request) => {
    const url = new URL(input.url);
    const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (url.pathname.endsWith("/auth/me")) return json({ user: { id: "u", username: "a", display_name: "A" }, role, space: { id: "s", name: "S", base_currency: "RUB" } });
    if (url.pathname.endsWith("/allocation/topup")) {
      const fromBrokers = Number(url.searchParams.get("from_brokers_minor") ?? 0);
      const free = fromBrokers > 0;
      return json({
        amount_minor: Number(url.searchParams.get("amount_minor")), from_brokers_minor: fromBrokers, worth_after_minor: 3_100_000_00,
        kinds: [
          { key: "shares", change_minor: 0, share_after: "33.87" },
          { key: "bonds", change_minor: free ? 200_000_00 : 65_346_55, share_after: free ? "25.81" : "21.46" },
          { key: "funds", change_minor: 2_970_29, share_after: "14.61" },
          { key: "metals", change_minor: free ? 197_029_71 : 31_683_16, share_after: free ? "6.36" : "1.02" },
          { key: "deposits", change_minor: 0, share_after: "19.35" },
          { key: "broker_cash", change_minor: free ? -300_000_00 : 0, share_after: free ? "0.00" : "9.68" },
        ],
      });
    }
    return json(a);
  });
}

function show(valuation: "liquid" | "full" = "liquid") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TargetsCard valuation={valuation} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("TargetsCard", () => {
  it("sets each kind against its target, the money to live on apart", async () => {
    answer("viewer", allocation);
    show();
    const rows = within(await screen.findByTestId("allocation-kinds")).getAllByTestId("allocation-row");
    expect(norm(rows[0].textContent)).toMatch(/Акции и расписки.*35 %.*Цель 30 % — больше на 5 п\.п\., на 150 000/);
    expect(norm(rows[1].textContent)).toMatch(/Облигации.*Цель 30 % — меньше на 10 п\.п\., на 300 000/);
    expect(norm(rows[2].textContent)).toMatch(/В цели: 15 %/);
    expect(norm(rows[5].textContent)).toMatch(/Свободные деньги у брокеров, RUB/);
    expect(norm(screen.getByTestId("allocation-outside").textContent)).toMatch(/деньги на жизнь .* 200 000/);
    expect(screen.queryByRole("button", { name: "Изменить цели" })).toBeNull();
  });

  it("follows the valuation picked on the page", async () => {
    answer("viewer", allocation);
    show("full");
    expect(norm((await screen.findByTestId("allocation")).textContent)).toMatch(/по полной оценке, с замороженными бумагами/);
    const asked = fetchMock.mock.calls.map(([r]) => new URL((r as Request).url)).filter((u) => u.pathname.endsWith("/allocation"));
    expect(asked.every((u) => u.searchParams.get("valuation") === "full")).toBe(true);
  });

  it("shows on the kinds where a top-up goes, the brokers' free money moved in", async () => {
    answer("viewer", allocation);
    show();
    fireEvent.change(await screen.findByLabelText("Новые деньги, RUB"), { target: { value: "100 000" } });
    expect(norm((await screen.findByTestId("allocation-topup-total")).textContent)).toBe("К вложению: 100 000 ₽.");
    const rows = within(screen.getByTestId("allocation-kinds")).getAllByTestId("allocation-row");
    expect(norm(rows[1].textContent)).toMatch(/Облигации.*\+65 347 ₽ → 21,46 % \(было 20 %, цель 30 %\) — ближе к цели на 1,46 п\.п\./);
    expect(norm(rows[0].textContent)).toMatch(/→ 33,87 % \(было 35 %, цель 30 %\) — ближе к цели на 1,13 п\.п\./);
    expect(within(rows[1]).getByTestId("allocation-added")).toBeTruthy();
    // A share that only thins as the worth grows gets words, not a mark.
    expect(within(rows[0]).queryByTestId("allocation-added")).toBeNull();
    expect(within(rows[0]).queryByTestId("allocation-removed")).toBeNull();
    expect(norm(screen.getByTestId("allocation-topup-legend").textContent)).toBe("прибавкацель");

    fireEvent.click(screen.getByLabelText(/Взять и свободные деньги у брокеров/));
    expect((screen.getByLabelText("Сколько взять у брокеров, RUB") as HTMLInputElement).value).toBe("300000");
    await waitFor(() =>
      expect(norm(screen.getByTestId("allocation-topup-total").textContent)).toBe("К вложению: 100 000 ₽ новых + 300 000 ₽ со счетов брокеров = 400 000 ₽."),
    );
    const asked = fetchMock.mock.calls.map(([r]) => new URL((r as Request).url)).filter((u) => u.pathname.endsWith("/topup")).at(-1)!;
    expect(asked.searchParams.get("amount_minor")).toBe("10000000");
    expect(asked.searchParams.get("from_brokers_minor")).toBe("30000000");
    const brokers = within(screen.getByTestId("allocation-kinds")).getAllByTestId("allocation-row")[5];
    expect(norm(brokers.textContent)).toMatch(/−300 000 ₽ → 0 % \(было 10 %, цель 0 %\) — в цели/);
    expect(within(brokers).getByTestId("allocation-removed")).toBeTruthy();
    expect(norm(screen.getByTestId("allocation-topup-legend").textContent)).toBe("прибавкауйдёт со счетов брокеровцель");

    fireEvent.change(screen.getByLabelText("Сколько взять у брокеров, RUB"), { target: { value: "500 000" } });
    expect(screen.getByText("У брокеров свободно только 300 000 ₽.")).toBeTruthy();
  });

  it("states targets that add up to 100", async () => {
    answer("owner", allocation);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Изменить цели" }));
    const dialog = await screen.findByTestId("allocation-dialog");
    expect((within(dialog).getByLabelText(/Акции и расписки/) as HTMLInputElement).value).toBe("30");
    fireEvent.change(within(dialog).getByLabelText(/Акции и расписки/), { target: { value: "25,5" } });
    expect(norm(screen.getByTestId("allocation-sum").textContent)).toBe("Всего: 95,5 % — нужно ровно 100 %.");
    const save = within(dialog).getByRole("button", { name: "Сохранить" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(within(dialog).getByLabelText(/Криптовалюта/), { target: { value: "4,5" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    const body = await put.json();
    expect(body.dimension).toBe("kind");
    expect(body.targets).toContainEqual({ key: "shares", percent: "25.50" });
    expect(body.targets).toContainEqual({ key: "crypto", percent: "4.50" });
    expect(body.targets).toContainEqual({ key: "broker_cash", percent: "0.00" });
  });

  it("adds a currency to the currency targets", async () => {
    answer("owner", allocation);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Задать цели по валютам" }));
    const dialog = await screen.findByTestId("allocation-dialog");
    fireEvent.change(within(dialog).getByLabelText(/Добавить валюту/), { target: { value: "cny" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Добавить" }));
    fireEvent.change(within(dialog).getByLabelText(/^RUB/), { target: { value: "60" } });
    fireEvent.change(within(dialog).getByLabelText(/^USD/), { target: { value: "25" } });
    fireEvent.change(within(dialog).getByLabelText(/^CNY/), { target: { value: "15" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
    const put = fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
    expect(await put.json()).toEqual({
      dimension: "currency",
      targets: [{ key: "RUB", percent: "60.00" }, { key: "USD", percent: "25.00" }, { key: "CNY", percent: "15.00" }],
    });
  });
});
