import { describe, expect, it } from "vitest";
import { readTableFile, readUpload } from "./read-table-file";

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

describe("readUpload", () => {
  it("sends a bank's PDF statement as it is, in base64", async () => {
    const file = new File([new Uint8Array([0x25, 0x50, 0x44, 0x46])], "Выписка по счёту.PDF");
    expect(await readUpload(file)).toEqual({ content: "JVBERg==", format: "pdf" });
  });
});
