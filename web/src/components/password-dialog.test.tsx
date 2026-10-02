import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { PasswordDialog } from "./password-dialog";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

let sent: { path: string; body: unknown }[] = [];

function serve(status: Record<string, number>) {
  sent = [];
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const req = input as Request;
    const path = new URL(req.url, "http://localhost").pathname;
    const text = await req.text();
    sent.push({ path, body: text ? JSON.parse(text) : null });
    const code = status[path] ?? 404;
    return new Response(code === 204 ? null : JSON.stringify({ error: "x" }), { status: code });
  });
}

function renderDialog() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <PasswordDialog open onOpenChange={() => {}} />
    </QueryClientProvider>,
  );
}

function fill(current: string, next: string, repeat = next) {
  fireEvent.change(screen.getByLabelText("Текущий пароль"), { target: { value: current } });
  fireEvent.change(screen.getByLabelText("Новый пароль"), { target: { value: next } });
  fireEvent.change(screen.getByLabelText("Новый пароль ещё раз"), { target: { value: repeat } });
}

afterEach(cleanup);

describe("PasswordDialog", () => {
  it("sends the current and the new password and says the other sessions ended", async () => {
    serve({ "/api/v1/auth/password": 204 });
    renderDialog();
    fill("secret123", "новый-пароль");
    fireEvent.click(screen.getByRole("button", { name: "Сменить пароль" }));
    expect(await screen.findByText("Пароль изменён. На других устройствах сеансы завершены.")).toBeInTheDocument();
    expect(sent).toEqual([{ path: "/api/v1/auth/password", body: { current_password: "secret123", new_password: "новый-пароль" } }]);
  });

  it("says the current password is wrong on a 400, and that the door is closed on a 429", async () => {
    serve({ "/api/v1/auth/password": 400 });
    renderDialog();
    fill("wrong", "новый-пароль");
    fireEvent.click(screen.getByRole("button", { name: "Сменить пароль" }));
    expect(await screen.findByText("Текущий пароль неверен")).toBeInTheDocument();
    cleanup();

    serve({ "/api/v1/auth/password": 429 });
    renderDialog();
    fill("wrong", "новый-пароль");
    fireEvent.click(screen.getByRole("button", { name: "Сменить пароль" }));
    expect(await screen.findByText("Слишком много неудачных попыток — подождите несколько минут")).toBeInTheDocument();
  });

  it("sends nothing for a new password under eight characters or one typed differently twice", () => {
    serve({});
    renderDialog();
    fill("secret123", "short");
    expect(screen.getByRole("button", { name: "Сменить пароль" })).toBeDisabled();
    fill("secret123", "новый-пароль", "новый-парол");
    expect(screen.getByRole("button", { name: "Сменить пароль" })).toBeDisabled();
    expect(screen.getByText("Пароли не совпадают")).toBeInTheDocument();
  });

  it("signs out everywhere else", async () => {
    serve({ "/api/v1/auth/sign-out-elsewhere": 204 });
    renderDialog();
    fireEvent.click(screen.getByRole("button", { name: "Выйти на других устройствах" }));
    expect(await screen.findByText("Готово: на других устройствах нужно войти заново.")).toBeInTheDocument();
    expect(sent.map((s) => s.path)).toEqual(["/api/v1/auth/sign-out-elsewhere"]);
  });
});
