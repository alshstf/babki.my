import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const STORAGE_KEY = "babki.displayCurrency";

// The store keeps its state in a module-level variable read once at import
// time, so each test that needs a *fresh* store (as if the page had just
// loaded) must reset the module registry and re-import.
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
    // Simulates Safari private browsing (older versions) / storage disabled,
    // where localStorage access throws on every call instead of returning
    // null. Spying on the instance (rather than a shared Storage.prototype)
    // matches the in-memory polyfill installed in test-setup.ts.
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

  it("toggling notifies other subscribers in the same tab", async () => {
    const { useDisplayCurrency } = await importFreshStore();
    // Two independent hook consumers, standing in for two components that
    // both read the store in the same tab.
    const { result: a } = renderHook(() => useDisplayCurrency());
    const { result: b } = renderHook(() => useDisplayCurrency());
    expect(a.current.mode).toBe("native");
    expect(b.current.mode).toBe("native");

    act(() => a.current.toggle());

    expect(a.current.mode).toBe("base");
    expect(b.current.mode).toBe("base");

    act(() => b.current.toggle());

    expect(a.current.mode).toBe("native");
    expect(b.current.mode).toBe("native");
  });

  it("syncs from a storage event fired by another tab", async () => {
    const { useDisplayCurrency } = await importFreshStore();
    const { result } = renderHook(() => useDisplayCurrency());
    expect(result.current.mode).toBe("native");

    act(() => {
      window.localStorage.setItem(STORAGE_KEY, "base");
      // storageArea is omitted: the store's listener only reads key/newValue,
      // and jsdom's StorageEvent constructor requires a real jsdom Storage
      // instance there — the in-memory polyfill from test-setup.ts isn't one.
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
