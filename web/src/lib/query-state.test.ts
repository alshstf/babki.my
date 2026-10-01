import { describe, expect, it } from "vitest";
import { queryState, refreshFailed } from "./query-state";

const held = { data: [1], isError: false, fetchStatus: "idle" as const };
const stale = { data: [1], isError: true, fetchStatus: "idle" as const };
const fetching = { data: undefined, isError: false, fetchStatus: "fetching" as const };
const paused = { data: undefined, isError: false, fetchStatus: "paused" as const };
const failed = { data: undefined, isError: true, fetchStatus: "idle" as const };

describe("queryState", () => {
  it("is ready once every query holds an answer, fresh or not", () => {
    expect(queryState(held, stale)).toBe("ready");
    expect(queryState()).toBe("ready");
  });
  it("is failed only when an answer is missing and asking failed", () => {
    expect(queryState(held, failed)).toBe("failed");
  });
  it("is offline for a request the browser has not sent, never empty or loading", () => {
    expect(queryState(held, paused)).toBe("offline");
  });
  it("is loading while an answer is on its way", () => {
    expect(queryState(fetching, held)).toBe("loading");
  });
  it("puts a failure ahead of a wait: one missing answer will not arrive", () => {
    expect(queryState(fetching, failed, paused)).toBe("failed");
  });
});

describe("refreshFailed", () => {
  it("is true when data on screen outlived a failed refresh", () => {
    expect(refreshFailed(held, stale)).toBe(true);
  });
  it("is false when nothing failed, and when the failure left no data at all", () => {
    expect(refreshFailed(held)).toBe(false);
    expect(refreshFailed(failed)).toBe(false);
  });
});
