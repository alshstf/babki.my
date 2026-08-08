import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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

function makeSession(role: SessionInfo["role"] = "owner"): SessionInfo {
  return {
    user: { id: "user-1", username: "alex", display_name: "Александр" },
    role,
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
function serve(removeStatus: number, removeBody: unknown, role: SessionInfo["role"] = "owner") {
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
    if (url.includes("/api/v1/auth/me")) return json(200, makeSession(role));
    return json(200, members);
  });
}

function renderPage(role: SessionInfo["role"] = "owner") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(["session"], makeSession(role));
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

// controlNames is every control this screen offers that DOES something, by the
// name a person reads off it: the buttons, and the role dropdowns — which are
// comboboxes rather than buttons and would otherwise be the half of the gating
// a button-only sweep quietly misses.
//
// The assertions below compare two roles' screens rather than checking a list
// of controls typed into the test, for the reason the T-Invest role tests take
// their route list from the router: a written list stays convincing on the day a
// new control appears that it does not mention, and a diff does not.
function controlNames(): string[] {
  return [...screen.queryAllByRole("button"), ...screen.queryAllByRole("combobox")]
    .map((el) => (el.getAttribute("aria-label") ?? el.textContent ?? "").trim())
    .filter((name) => name !== "")
    .sort();
}

function rowOf(name: string): HTMLElement {
  const row = screen.getByText(name).closest("tr");
  if (!row) throw new Error(`no table row for ${name}`);
  return row as HTMLElement;
}

// #14: who may change the family was verified by hand and by nothing else.
describe("FamilyPage — who may change the family", () => {
  it("offers a non-owner exactly the owner's screen minus every control that writes", async () => {
    serve(200, null, "owner");
    renderPage("owner");
    await screen.findByText("Мария");
    const asOwner = controlNames();

    cleanup();
    fetchMock.mockReset();

    serve(200, null, "editor");
    renderPage("editor");
    await screen.findByText("Мария");
    const asEditor = controlNames();

    // «редактор» is the role dropdown, named by the role currently selected in
    // it. Written out rather than derived: a control gated somewhere new has to
    // be added here deliberately.
    expect(asOwner.filter((name) => !asEditor.includes(name))).toEqual([
      "Добавить участника",
      "Удалить участника",
      "редактор",
    ]);
    expect(asEditor.filter((name) => !asOwner.includes(name))).toEqual([]);
    // Still a screen a member can read: the roles are there, as text.
    expect(within(rowOf("Мария")).getByText("редактор")).toBeInTheDocument();
  });

  it("leaves the owner's own row neither demotable nor removable", async () => {
    serve(200, null, "owner");
    renderPage("owner");
    await screen.findByText("Мария");

    // The owner is also the signed-in user here, which is not a coincidence to
    // be tidied away: a space has exactly one owner, so «the owner's row» and
    // «my own row» are the same row, and the two rules that hide the controls
    // (role is owner; the member is me) can only ever be observed together.
    const own = rowOf("Александр");
    expect(within(own).queryByRole("combobox")).toBeNull();
    expect(within(own).queryByRole("button", { name: "Удалить участника" })).toBeNull();
    expect(within(own).getByText("владелец")).toBeInTheDocument();

    // ...while the member who is neither has both, so the assertions above are
    // about the owner rather than about a page that offers nobody anything.
    const other = rowOf("Мария");
    expect(within(other).getByRole("combobox")).toBeInTheDocument();
    expect(within(other).getByRole("button", { name: "Удалить участника" })).toBeInTheDocument();
  });
});
