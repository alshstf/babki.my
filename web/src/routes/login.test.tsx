import type { ReactElement } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, onlineManager } from "@tanstack/react-query";
import "@/i18n";
import { LoginPage } from "./login";

// The API client captures globalThis.fetch once, when @/api/client is first
// imported (openapi-fetch: `fetch: baseFetch = globalThis.fetch`), so the
// double has to be in place *before* that import — hence vi.hoisted, which
// runs ahead of the import statements above.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a single one handed to mockResolvedValue works
// once and then throws, because a body can only be consumed once.
//
// `networkError` is what a browser with no connection does — fetch rejects and
// no status is ever read, the shape of a failure that carries no answer at all.
function serveLogin(route: { status?: number; body?: unknown; networkError?: boolean }) {
  fetchMock.mockImplementation(() => {
    if (route.networkError) return Promise.reject(new TypeError("Failed to fetch"));
    return Promise.resolve(
      new Response(JSON.stringify(route.body ?? null), {
        status: route.status ?? 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
  });
}

// Router hooks used by useLogin need a router context in full render;
// LoginPage itself only navigates on success, so a bare render is enough
// for the disabled-button contract. If rendering throws on useNavigate,
// wrap with a minimal RouterProvider instead of removing the test.
function wrap(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

const signInButton = () => screen.getByRole("button", { name: "Войти" });

// Fills both fields and presses the button, which is the only way to reach the
// mutation: the button is disabled until they carry something.
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
    // 401 is the only refusal the contract declares for this endpoint, and it
    // is the one case where naming the cause is naming what the server named.
    serveLogin({ status: 401, body: { error: "invalid credentials" } });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Неверный логин или пароль")).toBeInTheDocument();
  });

  it("says so when the browser reports no connection, and lets the reader try again", async () => {
    // The shape sign-out had until #88, still standing here. react-query's
    // default for mutations, networkMode "online", PAUSES this one while the
    // browser reports itself offline: nothing is sent, nothing fails, isError
    // stays false — so the alert never appears — and isPending stays true,
    // which disables the button that would try again. The reader presses
    // «Войти», sees nothing happen and nothing said, and has no way to tell a
    // dead connection from a slow one. Then, whenever the connection returns,
    // the held request goes out and the app signs in on its own.
    //
    // Signing in is worth only what it is worth AT THE MOMENT it is asked for,
    // so it is attempted whatever the browser believes about the network, and a
    // dead connection becomes an ordinary failure this screen already knows how
    // to report.
    onlineManager.setOnline(false);
    serveLogin({ networkError: true });
    wrap(<LoginPage />);

    attemptSignIn();

    expect(await screen.findByText("Не удалось войти. Попробуйте ещё раз")).toBeInTheDocument();
    // Attempted, not held: the browser's own idea of being offline does not get
    // to decide this one, and it is the request that reports the answer.
    expect(fetchMock).toHaveBeenCalled();
    // And the reader can try again — a locked button is the same silence.
    expect(signInButton()).not.toBeDisabled();
  });

  it("does not blame the password for a failure that was not about the password", async () => {
    // The other half of letting the request through: a failure that is not a
    // refusal must not be captioned as one. «Неверный логин или пароль» over a
    // dead connection or a broken server sends the reader to change a password
    // that was never wrong — a caption naming a cause it cannot know, which is
    // this branch's own subject. The contract declares 200 and 401 for this
    // endpoint and nothing else, so anything else is exactly the case where the
    // client has not been told why.
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
