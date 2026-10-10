// readTableFile reads an uploaded table as text. Exports are UTF-8 or, from
// Excel and older brokers' systems in Russia, Windows-1251; a file that does
// not decode as UTF-8 without loss is read as Windows-1251 instead.
export async function readTableFile(file: Blob): Promise<string> {
  const bytes = await file.arrayBuffer();
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return new TextDecoder("windows-1251").decode(bytes);
  }
}

// Upload is a file as the import sends it: text, or an Excel workbook in
// base64 for the server to read.
export type Upload = { content: string; format: "text" | "xlsx" };

// readUpload reads a file for the import: an Excel workbook (.xlsx) as it is,
// anything else — a CSV, a tracker's JSON export — as text.
export async function readUpload(file: File): Promise<Upload> {
  if (/\.xlsx$/i.test(file.name)) {
    return { content: toBase64(await file.arrayBuffer()), format: "xlsx" };
  }
  return { content: await readTableFile(file), format: "text" };
}

// isOldExcel is the binary .xls of Excel before 2007, which nothing here reads.
export function isOldExcel(file: File): boolean {
  return /\.xls$/i.test(file.name);
}

function toBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}
