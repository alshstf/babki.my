// decodeQrFromImage finds the QR code on a photo and returns its text, or null
// when there is none to read. The browser's own reader where it has one
// (Chrome on Android); elsewhere (Safari on an iPhone) the jsQR library, loaded
// only now, so the app does not carry it on every page.

// A phone's photo is ten or more megapixels; a receipt's code reads well at a
// fraction of that and the library is much quicker on it. Tried small first,
// then larger, for a code that fills little of the picture.
const SIDES = [1200, 2400];

interface DetectedBarcode {
  rawValue: string;
}
interface BarcodeDetectorLike {
  detect(source: ImageBitmapSource): Promise<DetectedBarcode[]>;
}
type BarcodeDetectorCtor = new (options: { formats: string[] }) => BarcodeDetectorLike;

export async function decodeQrFromImage(file: Blob): Promise<string | null> {
  const bitmap = await createImageBitmap(file);
  try {
    const Detector = (window as unknown as { BarcodeDetector?: BarcodeDetectorCtor }).BarcodeDetector;
    if (Detector) {
      try {
        const found = await new Detector({ formats: ["qr_code"] }).detect(bitmap);
        if (found[0]?.rawValue) return found[0].rawValue;
      } catch {
        // A detector that cannot read this format falls back to the library.
      }
    }
    const { default: jsQR } = await import("jsqr");
    for (const side of SIDES) {
      const scale = Math.min(1, side / Math.max(bitmap.width, bitmap.height));
      const width = Math.round(bitmap.width * scale);
      const height = Math.round(bitmap.height * scale);
      const canvas = document.createElement("canvas");
      canvas.width = width;
      canvas.height = height;
      const ctx = canvas.getContext("2d", { willReadFrequently: true });
      if (!ctx) return null;
      ctx.drawImage(bitmap, 0, 0, width, height);
      const code = jsQR(ctx.getImageData(0, 0, width, height).data, width, height, { inversionAttempts: "attemptBoth" });
      if (code?.data) return code.data;
      if (scale === 1) break;
    }
    return null;
  } finally {
    bitmap.close();
  }
}
