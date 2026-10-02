import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "@/i18n";
import { CapitalChart } from "./capital-chart";

const state = vi.hoisted(() => ({ points: [] as unknown[], asked: [] as string[] }));
const fetchMock = vi.hoisted(() => {
  const fn = vi.fn();
  globalThis.fetch = fn as unknown as typeof fetch;
  return fn;
});
fetchMock.mockImplementation(async (input: Request) => {
  state.asked.push(input.url);
  return new Response(JSON.stringify({ currency: "RUB", points: state.points }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
});

afterEach(() => {
  cleanup();
  state.asked = [];
});

function wrap() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <CapitalChart />
    </QueryClientProvider>,
  );
}

const point = (day: string, total: number, complete = true) => ({
  day,
  total_minor: total,
  complete,
  accounts: [],
});

describe("CapitalChart", () => {
  it("draws the year by month and marks a month valued only in part", async () => {
    state.points = [
      point("2026-08-31", 100_000_00),
      point("2026-09-30", 120_000_00, false),
      point("2026-10-03", 130_000_00),
    ];
    wrap();
    expect(await screen.findByTestId("capital-chart")).toBeTruthy();
    expect(state.asked[0]).toMatch(/\/api\/v1\/capital\?from=\d{4}-\d{2}-01&step=month$/);
    expect(screen.getByTestId("capital-point-2026-09-30").getAttribute("class")).toContain("stroke-amber-600");
    expect(screen.getByTestId("capital-point-2026-08-31").getAttribute("class")).toContain("fill-primary");
    expect(screen.getByTestId("capital-incomplete")).toBeTruthy();
    expect(screen.getByTestId("capital-point-2026-09-30").textContent).toContain("оценено не всё");
  });

  it("draws nothing with fewer than two points", async () => {
    state.points = [point("2026-10-03", 130_000_00)];
    wrap();
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId("capital-chart")).toBeNull();
  });
});
