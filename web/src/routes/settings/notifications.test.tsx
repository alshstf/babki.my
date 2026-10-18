import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { NotificationsSection } from "./notifications";
import type { PushSettings } from "@/api/push";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const kinds = ["card", "budget", "goal", "storage", "payout", "declared", "sync", "reconcile", "stale"];
const settings: PushSettings = { kinds: kinds.map((kind) => ({ kind, on: kind !== "budget" })), quiet_from: 21, quiet_to: 9 };

function answer(s: PushSettings) {
  fetchMock.mockImplementation(async (input: Request) => {
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    if (input.method === "PUT") {
      const body = await input.clone().json();
      return json({ kinds: kinds.map((kind) => ({ kind, on: !body.off.includes(kind) })), quiet_from: body.quiet_from, quiet_to: body.quiet_to });
    }
    return json(s);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <NotificationsSection />
    </QueryClientProvider>,
  );
}

const put = async () => {
  await waitFor(() => expect(fetchMock.mock.calls.some(([r]) => (r as Request).method === "PUT")).toBe(true));
  return fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === "PUT")!;
};

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

// Decision Р-61 Б.
describe("NotificationsSection", () => {
  it("lists every kind with whether it comes, and the quiet hours", async () => {
    answer(settings);
    show();
    const card = await screen.findByRole("checkbox", { name: "Платежи по кредитным картам" });
    expect(card.getAttribute("data-state")).toBe("checked");
    expect(screen.getByRole("checkbox", { name: "Бюджет: лимит почти исчерпан или превышен" }).getAttribute("data-state")).toBe("unchecked");
    expect(screen.getByRole("checkbox", { name: "Объявлены дивиденды по вашим бумагам" })).toBeTruthy();
    expect(screen.getByRole("combobox", { name: "с" }).textContent).toBe("21:00");
    expect(screen.getByRole("combobox", { name: "до" }).textContent).toBe("09:00");
    expect(screen.getByTestId("notifications").textContent).toContain("В эти часы уведомления не приходят");
  });

  it("turns a kind off, keeping the others and the hours", async () => {
    answer(settings);
    show();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Цены или курсы давно не обновлялись" }));
    expect(await (await put()).json()).toEqual({ off: ["budget", "stale"], quiet_from: 21, quiet_to: 9 });
  });

  it("says equal hours are never quiet", async () => {
    answer({ ...settings, quiet_from: 8, quiet_to: 8 });
    show();
    expect(await screen.findByText("Часы совпадают — уведомления приходят в любое время.")).toBeTruthy();
  });
});
