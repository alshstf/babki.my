import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import "@/i18n";
import { LoginPage } from "./login";

// The API client captures globalThis.fetch on first import, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
// `networkError` makes fetch reject, as a browser with no connection does.
function serveLogin(route: {
  status?: number;
  body?: unknown;
  networkError?: boolean;
  headers?: Record<string, string>;
}) {
  fetchMock.mockImplementation(() => {
    if (route.networkError) return Promise.reject(new TypeError("Failed to fetch"));
    return Promise.resolve(
      new Response(JSON.stringify(route.body ?? null), {
        status: route.status ?? 200,
        headers: { "Content-Type": "application/json", ...route.headers },
      }),
    );
  });
}

// No router: LoginPage navigates only on success, which these tests do
// not reach through a route.
function wrap(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

const signInButton = () => screen.getByRole("button", { name: "Войти" });

// Fills both fields and presses the button, which is disabled until
// both carry something.
function attemptSignIn() {
  fireEvent.change(screen.getByLabelText("Логин"), { target: { value: "demo" } });
  fireEvent.change(screen.getByLabelText("Пароль"), { target: { value: "demo1234" } });
  fireEvent.click(signInButton());
}

afterEach(() => {
  onlineManager.setOnline(true);
  fetchMock.mockReset();
  cleanup();
});

describe("LoginPage", () => {
  it("renders form with disabled submit until filled", () => {
    wrap(<LoginPage />);
    expect(screen.getByLabelText("Логин")).toBeInTheDocument();
    expect(screen.getByLabelText("Пароль")).toBeInTheDocument();
    expect(signInButton()).toBeDisabled();
  });

  it("says the credentials were refused when that is what the server said", async () => {
    // 401 is the one refusal the contract declares, so naming the cause is
    // naming the server's.
    serveLogin({ status: 401, body: { error: "invalid credentials" } });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Неверный логин или пароль")).toBeInTheDocument();
  });

  it("says the door is closed, and for how long, after too many wrong passwords", async () => {
    // 429 is not a wrong password: while the lock stands even the right one
    // is refused. The form gives the server's wait in minutes, rounded up.
    serveLogin({
      status: 429,
      body: { error: "too many sign-in attempts, try again later" },
      headers: { "Retry-After": "95" },
    });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText(/Вход с этим логином закрыт ещё на 2 мин/)).toBeInTheDocument();
    expect(screen.queryByText("Неверный логин или пароль")).not.toBeInTheDocument();
    expect(screen.queryByText("Не удалось войти. Попробуйте ещё раз")).not.toBeInTheDocument();
  });

  it("does not invent a wait the server did not state", async () => {
    serveLogin({ status: 429, body: { error: "too many sign-in attempts, try again later" } });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText(/Вход с этим логином временно закрыт/)).toBeInTheDocument();
    expect(screen.queryByText(/мин —/)).not.toBeInTheDocument();
  });

  it("says so when the browser reports no connection, and lets the reader try again", async () => {
    // The default networkMode "online" would pause the mutation offline: no
    // alert, a disabled button, and a sign-in later on its own. Signing in is
    // attempted whatever the browser believes, so a dead connection is an
    // ordinary failure.
    onlineManager.setOnline(false);
    serveLogin({ networkError: true });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Не удалось войти. Попробуйте ещё раз")).toBeInTheDocument();
    // Attempted, not held.
    expect(fetchMock).toHaveBeenCalled();
    // And the reader can try again — a locked button is the same silence.
    expect(signInButton()).not.toBeDisabled();
  });

  it("does not blame the password for a failure that was not about the password", async () => {
    // A failure that is not a refusal must not be captioned as a wrong
    // password: the contract declares only 200 and 401.
    serveLogin({ networkError: true });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Не удалось войти. Попробуйте ещё раз")).toBeInTheDocument();
    expect(screen.queryByText("Неверный логин или пароль")).not.toBeInTheDocument();
  });

  it("does not blame the password for a server error either", async () => {
    serveLogin({ status: 500, body: { error: "internal error" } });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Не удалось войти. Попробуйте ещё раз")).toBeInTheDocument();
    expect(screen.queryByText("Неверный логин или пароль")).not.toBeInTheDocument();
  });
});

// The server accepted the sign-in but the browser did not keep the
// HTTPS-only cookie on plain http. The form says so and what to do.
describe("LoginPage — a sign-in the browser did not keep", () => {
  it("names the cause when the next request arrives signed out", async () => {
    fetchMock.mockImplementation((input: RequestInfo | URL) => {
      const url = input instanceof Request ? input.url : String(input);
      const signedOut = url.includes("/auth/me");
      return Promise.resolve(
        new Response(JSON.stringify(signedOut ? { error: "authentication required" } : { role: "owner" }), {
          status: signedOut ? 401 : 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText(/браузер не сохранил вход/)).toBeInTheDocument();
    expect(screen.queryByText("Неверный логин или пароль")).toBeNull();
  });
});
