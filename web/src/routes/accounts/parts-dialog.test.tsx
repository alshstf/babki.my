import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { PartsDialog } from "./parts-dialog";
import type { Operation } from "@/api/operations";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const categories = [
  { id: "c-food", kind: "expense", name: "Продукты", parent_id: null, archived: false, position: 1 },
  { id: "c-home", kind: "expense", name: "Дом", parent_id: null, archived: false, position: 2 },
];

fetchMock.mockImplementation((input: Request) => {
  const path = new URL(input.url).pathname;
  const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
  if (path.endsWith("/categories")) return json(categories);
  if (path.endsWith("/parts")) return json({ id: "op-1", account_id: "card" });
  return json([]);
});

// The supermarket's receipt of decision Р-36: 2 340,90 ₽ filed as groceries.
const row = (parts: Operation["parts"] = []) =>
  ({
    id: "op-1", account_id: "card", type: "withdrawal", occurred_on: "2026-10-09", amount_minor: -234090, currency: "RUB",
    category_id: "c-food", parts,
  }) as unknown as Operation;

function open(op: Operation, onClose = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <PartsDialog operation={op} onClose={onClose} />
    </QueryClientProvider>,
  );
  return onClose;
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");
const sent = () => fetchMock.mock.calls.map(([r]) => r as Request).filter((r) => new URL(r.url).pathname.endsWith("/parts"));

afterEach(() => {
  cleanup();
  fetchMock.mockClear();
});

describe("PartsDialog", () => {
  it("leaves the rest to the row's own category and saves the parts", async () => {
    const onClose = open(row());
    Element.prototype.scrollIntoView ??= () => {};
    const part = screen.getAllByTestId("parts-row")[0];
    fireEvent.keyDown(within(part).getByRole("combobox"), { key: "Enter" });
    fireEvent.click(await screen.findByRole("option", { name: "Дом" }));
    fireEvent.change(screen.getByLabelText("Сумма части 2"), { target: { value: "649,98" } });
    expect(norm(screen.getByTestId("parts-rest").textContent)).toBe("1 690,92 ₽");
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    const put = sent()[0];
    expect(put.method).toBe("PUT");
    expect(await put.json()).toEqual({
      parts: [{ category_id: "c-food", amount_minor: 169092 }, { category_id: "c-home", amount_minor: 64998 }],
    });
  });

  it("refuses parts larger than the row", async () => {
    open(row());
    fireEvent.change(screen.getByLabelText("Сумма части 2"), { target: { value: "3000" } });
    expect(await screen.findByText("Части больше всей траты — уменьшите их.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Сохранить" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("makes a split row whole again", async () => {
    const onClose = open(row([{ category_id: "c-food", amount_minor: 169092 }, { category_id: "c-home", amount_minor: 64998 }]));
    expect((screen.getByLabelText("Сумма части 2") as HTMLInputElement).value).toBe("649.98");
    fireEvent.click(screen.getByRole("button", { name: "Объединить" }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(sent()[0].method).toBe("DELETE");
  });
});
