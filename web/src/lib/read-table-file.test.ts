import { describe, expect, it } from "vitest";
import { readTableFile } from "./read-table-file";

describe("readTableFile", () => {
  it("reads UTF-8 as UTF-8", async () => {
    expect(await readTableFile(new Blob(["Дата;Сумма"]))).toBe("Дата;Сумма");
  });

  it("reads a Windows-1251 export as Windows-1251", async () => {
    // «Дата» in Windows-1251: not valid UTF-8.
    const bytes = new Uint8Array([0xc4, 0xe0, 0xf2, 0xe0]);
    expect(await readTableFile(new Blob([bytes]))).toBe("Дата");
  });
});
