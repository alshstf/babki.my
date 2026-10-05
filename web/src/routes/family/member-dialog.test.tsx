import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { MemberDialog } from "./member-dialog";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// A fresh Response per call: a body can be read only once.
function serve(status: number, body: unknown) {
  fetchMock.mockImplementation(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      }),
    ),
  );
}

function fillAndSubmit() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemberDialog open onOpenChange={() => {}} />
    </QueryClientProvider>,
  );
  fireEvent.change(screen.getByLabelText("Логин"), { target: { value: "maria" } });
  fireEvent.change(screen.getByLabelText("Имя участника"), { target: { value: "Мария" } });
  fireEvent.change(screen.getByLabelText("Пароль"), { target: { value: "12345678" } });
  fireEvent.click(screen.getByRole("button", { name: "Создать" }));
}

afterEach(() => {
  fetchMock.mockReset();
});

// «Логин уже занят» is chosen by status (409 on POST /api/v1/members),
// which the API promises, not by the prose, which it does not.
describe("MemberDialog — which refusal it recognises", () => {
  it("names the taken login on a conflict", async () => {
    // The server's refusal reworded: recognising it by prose would pass only
    // by accident.
    serve(409, { error: "that username is already in use" });
    fillAndSubmit();

    expect(await screen.findByText("Логин уже занят")).toBeInTheDocument();
  });

  it("does not claim the login is taken when the server said something else", async () => {
    serve(500, { error: "internal error" });
    fillAndSubmit();

    expect(
      await screen.findByText(/Не удалось выполнить действие/),
    ).toBeInTheDocument();
    expect(screen.queryByText("Логин уже занят")).toBeNull();
    expect(document.body.textContent).not.toContain("internal error");
  });
});
