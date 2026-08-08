import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { FamilyPage } from "./index";
import type { SessionInfo } from "@/api/session";
import type { MemberInfo } from "@/api/members";

// openapi-fetch captures globalThis.fetch at import time
// (`fetch: baseFetch = globalThis.fetch`), so the double has to be installed
// *before* the imports above run — hence vi.hoisted.
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

const members: MemberInfo[] = [
  { id: "user-1", username: "alex", display_name: "Александр", role: "owner" },
  { id: "user-2", username: "maria", display_name: "Мария", role: "editor" },
];

function makeSession(): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Александр" },
    role: "owner",
    space_id: "space-1",
    space_name: "Family",
    base_currency: "RUB",
    tax_residency: "RU",
    cost_basis_rules: {
      country: "RU",
      method: "fifo",
      perimeter: "account",
      supported: true,
      notices: [],
    },
  };
}

// Method-aware: the removal is a DELETE to the very path the member list is
// read from, so a mock keyed on the path alone would answer it with the list —
// a 200, i.e. a success — and the dialog under test would never see a refusal.
// A fresh Response per call, since a body can only be consumed once.
function serve(removeStatus: number, removeBody: unknown) {
  fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    const method = input instanceof Request ? input.method : (init?.method ?? "GET");
    const json = (status: number, body: unknown) =>
      Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { "Content-Type": "application/json" },
        }),
      );
    if (method === "DELETE") return json(removeStatus, removeBody);
    // The seeded session is refetched on mount; answering that with the member
    // list would hand the page an array where it expects a session object.
    if (url.includes("/api/v1/auth/me")) return json(200, makeSession());
    return json(200, members);
  });
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], makeSession());
  return render(
    <QueryClientProvider client={qc}>
      <FamilyPage />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  fetchMock.mockReset();
});

// #95: this confirmation printed the server's own error body, which is English
// and written for a log rather than for the owner of a family space.
describe("FamilyPage — a removal the server refused", () => {
  it("says it in Russian and does not repeat the server's own words", async () => {
    serve(400, { error: "validation: the owner cannot be removed" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Удалить участника" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Удалить участника" }));

    expect(await within(dialog).findByText("Не удалось удалить участника")).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("cannot be removed");
  });

  // #21: the alert lives on the mutation, and Cancel is a plain button that
  // clears removeTarget — Radix calls onOpenChange only for its OWN dismiss
  // triggers, so the reset written there never ran on this path. The next
  // confirmation therefore opened already claiming a removal had failed, about
  // an attempt nobody had made yet, and for whichever member was picked next.
  it("opens clean afterwards instead of carrying the refusal to the next member", async () => {
    serve(400, { error: "validation: the owner cannot be removed" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Удалить участника" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Удалить участника" }));
    expect(await within(dialog).findByText("Не удалось удалить участника")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Отмена" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    fireEvent.click(screen.getByRole("button", { name: "Удалить участника" }));
    const reopened = await screen.findByRole("dialog");
    expect(within(reopened).queryByText("Не удалось удалить участника")).toBeNull();
  });
});
