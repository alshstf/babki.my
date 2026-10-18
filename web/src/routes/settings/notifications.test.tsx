import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { NotificationsSection } from "./notifications";
import type { PushSettings } from "@/api/push";
import type { TelegramStatus } from "@/api/telegram";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = () => {};
}

const kinds = ["card", "budget", "goal", "storage", "payout", "declared", "sync", "reconcile", "stale"];
const settings: PushSettings = {
  kinds: kinds.map((kind) => ({ kind, on: kind !== "budget", telegram: kind !== "stale" })),
  amounts: true,
  quiet_from: 21,
  quiet_to: 9,
};
const noBot: TelegramStatus = { available: true, can_manage: true, bot: null, linked: false };
const withBot: TelegramStatus = { ...noBot, bot: { username: "babki_family_bot", token_last4: "0000" } };

function answer(s: PushSettings, tg: TelegramStatus = noBot) {
  fetchMock.mockImplementation(async (input: Request) => {
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    const path = new URL(input.url).pathname;
    if (path === "/api/v1/telegram/link") {
      return json({ code: "abcdefghabcdefgh", url: "https://t.me/babki_family_bot?start=abcdefghabcdefgh", expires_at: "2026-10-19T10:15:00Z" });
    }
    if (path === "/api/v1/telegram/bot" && input.method === "PUT") return json(withBot);
    if (path === "/api/v1/telegram") return json(tg);
    if (input.method === "PUT") {
      const body = await input.clone().json();
      return json({
        kinds: kinds.map((kind) => ({ kind, on: !body.off.includes(kind), telegram: !body.telegram_off.includes(kind) })),
        amounts: body.amounts,
        quiet_from: body.quiet_from,
        quiet_to: body.quiet_to,
      });
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

const sent = async (method: string, path: string) => {
  const find = () => fetchMock.mock.calls.map(([r]) => r as Request).find((r) => r.method === method && new URL(r.url).pathname === path);
  await waitFor(() => expect(find()).toBeTruthy());
  return find()!;
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
    // Without a bot, no Telegram column and no sums' choice.
    expect(screen.queryByRole("checkbox", { name: "Платежи по кредитным картам — Telegram" })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "Суммы в сообщениях Telegram" })).toBeNull();
  });

  it("turns a kind off, keeping the others and the hours", async () => {
    answer(settings);
    show();
    fireEvent.click(await screen.findByRole("checkbox", { name: "Цены или курсы давно не обновлялись" }));
    expect(await (await sent("PUT", "/api/v1/push/settings")).json()).toEqual({ off: ["budget", "stale"], telegram_off: ["stale"], amounts: true, quiet_from: 21, quiet_to: 9 });
  });

  it("says equal hours are never quiet", async () => {
    answer({ ...settings, quiet_from: 8, quiet_to: 8 });
    show();
    expect(await screen.findByText("Часы совпадают — уведомления приходят в любое время.")).toBeTruthy();
  });

  it("lets the owner give the space a bot", async () => {
    answer(settings);
    show();
    fireEvent.change(await screen.findByRole("textbox", { name: "Токен бота" }), { target: { value: " 123456:the-family-bot-token-0000 " } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    expect(await (await sent("PUT", "/api/v1/telegram/bot")).json()).toEqual({ token: "123456:the-family-bot-token-0000" });
    expect(await screen.findByText("Бот @babki_family_bot (токен …0000)")).toBeTruthy();
  });

  it("gives a link to tie the chat, and takes kinds and sums for Telegram", async () => {
    answer(settings, withBot);
    show();
    fireEvent.click(await screen.findByRole("button", { name: "Привязать Telegram" }));
    const link = await screen.findByTestId("telegram-link");
    expect(link.getAttribute("href")).toBe("https://t.me/babki_family_bot?start=abcdefghabcdefgh");
    fireEvent.click(screen.getByRole("checkbox", { name: "Суммы в сообщениях Telegram" }));
    expect(await (await sent("PUT", "/api/v1/push/settings")).json()).toMatchObject({ telegram_off: ["stale"], amounts: false });
  });

  it("says the chat is tied", async () => {
    answer(settings, { ...withBot, can_manage: false, linked: true });
    show();
    expect(await screen.findByText("Ваш чат с ботом привязан.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Отключить бота" })).toBeNull();
  });
});
