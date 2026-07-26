import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { LoginPage } from "./login";

// Router hooks used by useLogin need a router context in full render;
// LoginPage itself only navigates on success, so a bare render is enough
// for the disabled-button contract. If rendering throws on useNavigate,
// wrap with a minimal RouterProvider instead of removing the test.
function wrap(ui: ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

describe("LoginPage", () => {
  it("renders form with disabled submit until filled", () => {
    wrap(<LoginPage />);
    expect(screen.getByLabelText("Логин")).toBeInTheDocument();
    expect(screen.getByLabelText("Пароль")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Войти" })).toBeDisabled();
  });
});
