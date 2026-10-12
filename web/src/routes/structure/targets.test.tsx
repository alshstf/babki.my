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
