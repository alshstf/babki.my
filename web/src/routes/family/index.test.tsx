import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { FamilyPage } from "./index";
import type { SessionInfo } from "@/api/session";
import type { MemberInfo } from "@/api/members";

// openapi-fetch captures globalThis.fetch at import time, so the double is
// installed with vi.hoisted, ahead of the imports.
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
    full_valuation: "nav_and_foreign",
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

// Method-aware: the removal DELETE goes to the list's path, so a path-only
// mock would answer it with a 200. A fresh Response per call.
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
    // The seeded session is refetched on mount and must get a session.
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

// #95: the confirmation printed the server's English log prose.
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

  // #21: Cancel is a plain button and Radix calls onOpenChange only for its
  // own dismiss triggers, so the refusal stayed for the next member.
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

// Every control that does something, by the name a person reads: buttons
// and the role dropdowns (comboboxes). The tests compare two roles'
// screens, so a new control shows up in the diff by itself.
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

    // «Роль» is the role dropdown. Written out so a newly gated control is
    // added deliberately.
    expect(asOwner.filter((name) => !asEditor.includes(name))).toEqual([
      "Добавить участника",
      "Роль",
      "Удалить участника",
    ]);
    expect(asEditor.filter((name) => !asOwner.includes(name))).toEqual([]);
    // Still a screen a member can read: the roles are there, as text.
    expect(within(rowOf("Мария")).getByText("редактор")).toBeInTheDocument();
  });

  it("leaves the owner's own row neither demotable nor removable", async () => {
    serve(200, null, "owner");
    renderPage("owner");
    await screen.findByText("Мария");

    // The owner is the signed-in user: a space has one owner, so the two
    // hiding rules (role is owner; the member is me) are observed together.
    const own = rowOf("Александр");
    expect(within(own).queryByRole("combobox")).toBeNull();
    expect(within(own).queryByRole("button", { name: "Удалить участника" })).toBeNull();
    expect(within(own).getByText("владелец")).toBeInTheDocument();

    // ...while another member has both, so the page does offer controls.
    const other = rowOf("Мария");
    expect(within(other).getByRole("combobox")).toBeInTheDocument();
    expect(within(other).getByRole("button", { name: "Удалить участника" })).toBeInTheDocument();
  });
});
