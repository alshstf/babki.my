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
