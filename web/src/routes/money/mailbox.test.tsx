import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { MailboxSection } from "./mailbox";
import type { Mailbox } from "@/api/receipts";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const stated: Mailbox = {
  host: "imap.yandex.ru", port: 993, username: "cheki@example.ru", folder: "INBOX",
  checked_at: "2026-10-11T00:30:00Z", problem: "", last_found: 2,
};

function answer(box: Mailbox | null) {
  fetchMock.mockImplementation((input: Request) => {
    const url = new URL(input.url);
    const json = (body: unknown, status = 200) =>
      Promise.resolve(new Response(body === null ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
    if (url.pathname.endsWith("/mailbox/check")) return json({ mailbox: stated, result: { found: 3, attached: 1, waiting: 2, enriched: 0, known: 0, split: 0 } });
    if (url.pathname.endsWith("/mailbox") && input.method === "GET") return box ? json(box) : json({ error: "not found" }, 404);
    if (url.pathname.endsWith("/mailbox") && input.method === "DELETE") return json(null, 204);
    if (url.pathname.endsWith("/mailbox")) return json(stated);
    return json([]);
  });
}

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MailboxSection />
    </QueryClientProvider>,
  );
}

const sent = (method: string) => fetchMock.mock.calls.map(([r]) => r as Request).filter((r) => r.method === method);

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("MailboxSection", () => {
  it("states a new box with its app password", async () => {
    answer(null);
    show();
    await screen.findByTestId("mailbox-form");
    expect((screen.getByLabelText("Сервер IMAP") as HTMLInputElement).value).toBe("imap.yandex.ru");
    fireEvent.change(screen.getByLabelText("Адрес ящика"), { target: { value: "cheki@example.ru" } });
    const save = screen.getByRole("button", { name: "Сохранить" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Пароль приложения"), { target: { value: "app-pass" } });
    fireEvent.click(save);
    await waitFor(() => expect(sent("PUT")).toHaveLength(1));
    expect(await sent("PUT")[0].json()).toEqual({ host: "imap.yandex.ru", port: 993, username: "cheki@example.ru", folder: "INBOX", password: "app-pass" });
  });

  it("tells the last reading and reads now", async () => {
    answer(stated);
    show();
    expect((await screen.findByTestId("mailbox-status")).textContent).toMatch(/Чеков в новых письмах: 2\./);
    fireEvent.click(screen.getByRole("button", { name: "Проверить сейчас" }));
    expect((await screen.findByTestId("mailbox-result")).textContent).toBe("Чеков в новых письмах: 3.");
  });

  it("says what went wrong and keeps the password when edited", async () => {
    answer({ ...stated, problem: "login" });
    show();
    expect((await screen.findByTestId("mailbox-status")).textContent).toMatch(/Нужен пароль приложения/);
    fireEvent.click(screen.getByRole("button", { name: "Изменить" }));
    fireEvent.change(screen.getByLabelText("Папка"), { target: { value: "Чеки" } });
    fireEvent.click(screen.getByRole("button", { name: "Сохранить" }));
    await waitFor(() => expect(sent("PUT")).toHaveLength(1));
    expect(await sent("PUT")[0].json()).toMatchObject({ folder: "Чеки", password: null });
  });
});
