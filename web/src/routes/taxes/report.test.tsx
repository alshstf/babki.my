import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { ForeignReport } from "./report";
import type { TaxYearForeignReport } from "@/api/tax-year";

const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});

// Two brokers abroad in 2026 (decision Р-45): Freedom in Kazakhstan with
// 400 000 ₽ in — no report; IBKR with 640 000 ₽ in — the report is due.
const report: TaxYearForeignReport = {
  year: 2026, supported: true, currency: "RUB", free_up_to_minor: 600_000_00, due_by: "2027-06-01", until: "2026-12-31",
  accounts: [
    {
      account_id: "a-kz", name: "Freedom KZ", user_id: "u-alex", country: "KZ", country_code: "398",
      cash: [{ currency: "USD", currency_code: "840", start_minor: 0, in_minor: 500_000, out_minor: 100_000, end_minor: 400_000,
        start_thousands: "0.000", in_thousands: "5.000", out_thousands: "1.000", end_thousands: "4.000" }],
      papers_start: [], papers_end: [{ currency: "USD", currency_code: "840", minor: 300_000, thousands: "3.000" }],
      unpriced: 0, in_minor: 400_000_00, nothing_in: false, verdict: "free",
    },
    {
      account_id: "a-us", name: "IBKR", user_id: "u-alex", country: "US", country_code: "840",
      cash: [{ currency: "USD", currency_code: "840", start_minor: 100_000, in_minor: 800_000, out_minor: 0, end_minor: 900_000,
        start_thousands: "1.000", in_thousands: "8.000", out_thousands: "0.000", end_thousands: "9.000" }],
      papers_start: [], papers_end: [], unpriced: 0, in_minor: 640_000_00, nothing_in: false, verdict: "due",
    },
  ],
};

function show(body: TaxYearForeignReport) {
  fetchMock.mockImplementation((input: Request) => {
    const json = (b: unknown) => Promise.resolve(new Response(JSON.stringify(b), { status: 200, headers: { "Content-Type": "application/json" } }));
    if (new URL(input.url).pathname.endsWith("/members")) return json([{ id: "u-alex", username: "a", display_name: "Алекс", role: "owner" }]);
    return json(body);
  });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <ForeignReport year={2026} />
    </QueryClientProvider>,
  );
}

const norm = (s: string | null) => (s ?? "").replace(/\s/g, " ");

afterEach(() => {
  cleanup();
  fetchMock.mockReset();
});

describe("ForeignReport", () => {
  it("lays out each account's money and papers and says whether the report is due", async () => {
    show(report);
    expect(norm((await screen.findByTestId("tax-report")).textContent)).toMatch(/^Отчёт о движении средств за 2026 год — до 01\.06\.2027/);
    const [kz, us] = screen.getAllByTestId("tax-report-account");
    expect(norm(within(kz).getByTestId("tax-report-verdict").textContent)).toBe(
      "Отчёт не нужен: брокер в стране ЕАЭС, за год зачислено 400 000,00 ₽ по курсу ЦБ на 31.12.2026 — не больше 600 000,00 ₽.",
    );
    expect(norm(within(kz).getByTestId("tax-report-currency").textContent)).toBe("USD (840)0.0005.0001.0004.000");
    expect(norm(within(kz).getByTestId("tax-report-papers").textContent)).toMatch(/на начало года — 0 тыс\. · на конец — 3\.000 USD тыс\./);
    expect(norm(within(us).getByTestId("tax-report-verdict").textContent)).toMatch(/^Отчёт нужен: за год зачислено 640 000,00 ₽/);
    expect(within(us).queryByTestId("tax-report-papers")).toBeNull();
  });

  it("says the papers no price struck, even with none valued", async () => {
    show({ ...report, accounts: [{ ...report.accounts[1], unpriced: 3 }] });
    const papers = await screen.findByTestId("tax-report-papers");
    expect(norm(papers.textContent)).toMatch(/Бумаг без цены на одну из дат — не вошли в стоимость: 3\./);
  });

  it("says a running year is counted to today", async () => {
    show({ ...report, until: "2026-10-14" });
    expect(norm((await screen.findByTestId("tax-report-running")).textContent)).toMatch(/^Год ещё идёт — посчитано на 14\.10\.2026/);
    const [kz] = screen.getAllByTestId("tax-report-account");
    expect(within(kz).getByRole("columnheader", { name: "На 14.10.2026, тыс." })).toBeTruthy();
    expect(norm(within(kz).getByTestId("tax-report-papers").textContent)).toMatch(/на 14\.10\.2026 — 3\.000 USD тыс\./);
  });

  it("is not there without accounts abroad", async () => {
    show({ ...report, accounts: [] });
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByTestId("tax-report")).toBeNull();
  });
});
