import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const STORAGE_KEY = "babki.displayCurrency";

// The store reads its state once at import, so a fresh store needs a
// fresh module.
async function importFreshStore() {
  vi.resetModules();
  return import("./display-currency");
}

describe("useDisplayCurrency", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("defaults to native mode when localStorage is empty", async () => {
    const { useDisplayCurrency } = await importFreshStore();
    const { result } = renderHook(() => useDisplayCurrency());
    expect(result.current.mode).toBe("native");
  });

  it("persists a chosen mode and a freshly loaded store reads it back", async () => {
    const { useDisplayCurrency: useFirst } = await importFreshStore();
    const { result: first } = renderHook(() => useFirst());

    act(() => first.current.setMode("base"));

    expect(window.localStorage.getItem(STORAGE_KEY)).toBe("base");

    const { useDisplayCurrency: useSecond } = await importFreshStore();
    const { result: second } = renderHook(() => useSecond());
    expect(second.current.mode).toBe("base");
  });

  it("falls back to native when localStorage holds garbage", async () => {
    window.localStorage.setItem(STORAGE_KEY, "rubles-please");
    const { useDisplayCurrency } = await importFreshStore();
    const { result } = renderHook(() => useDisplayCurrency());
    expect(result.current.mode).toBe("native");
  });

  it("falls back to native and does not throw when localStorage.getItem throws", async () => {
    // Storage that throws on every call (old Safari private mode). Spied on
    // the instance, matching test-setup.ts's in-memory polyfill.
    const spy = vi.spyOn(window.localStorage, "getItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    try {
      const { useDisplayCurrency } = await importFreshStore();
      let result: { current: { mode: string } } | undefined;
      expect(() => {
        result = renderHook(() => useDisplayCurrency()).result;
      }).not.toThrow();
      expect(result?.current.mode).toBe("native");
    } finally {
      spy.mockRestore();
    }
  });

  it("does not throw when localStorage.setItem throws, and keeps the in-memory update", async () => {
    const spy = vi.spyOn(window.localStorage, "setItem").mockImplementation(() => {
      throw new DOMException("blocked", "QuotaExceededError");
    });
    try {
      const { useDisplayCurrency } = await importFreshStore();
      const { result } = renderHook(() => useDisplayCurrency());
      expect(() => act(() => result.current.setMode("base"))).not.toThrow();
      expect(result.current.mode).toBe("base");
    } finally {
      spy.mockRestore();
    }
  });

  it("a mode change notifies other subscribers in the same tab", async () => {
    const { useDisplayCurrency } = await importFreshStore();
    // Two consumers in one tab.
    const { result: a } = renderHook(() => useDisplayCurrency());
    const { result: b } = renderHook(() => useDisplayCurrency());
    expect(a.current.mode).toBe("native");
    expect(b.current.mode).toBe("native");

    act(() => a.current.setMode("base"));

    expect(a.current.mode).toBe("base");
    expect(b.current.mode).toBe("base");

    act(() => b.current.setMode("native"));

    expect(a.current.mode).toBe("native");
    expect(b.current.mode).toBe("native");
  });

  it("syncs from a storage event fired by another tab", async () => {
    const { useDisplayCurrency } = await importFreshStore();
    const { result } = renderHook(() => useDisplayCurrency());
    expect(result.current.mode).toBe("native");

    act(() => {
      window.localStorage.setItem(STORAGE_KEY, "base");
      // No storageArea: the listener reads only key/newValue, and jsdom's
      // constructor rejects the polyfill there.
      window.dispatchEvent(
        new StorageEvent("storage", {
          key: STORAGE_KEY,
          newValue: "base",
        }),
      );
    });

    expect(result.current.mode).toBe("base");
  });
});
