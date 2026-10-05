import { describe, expect, it } from "vitest";
import ru from "@/i18n/ru.json";
import { unnameableGap } from "./unnameable-gap";

describe("unnameableGap", () => {
  it("hands back the caller's fallback and invents nothing", () => {
    // A value off the wire missing from this build's union reaches the
    // screen as the caller's sentence, never a guessed cause. The cast is what
    // a client behind the server gets: JSON typed by assertion.
    expect(unnameableGap("no_rate_next_tuesday" as never, "запасная фраза")).toBe("запасная фраза");
  });
});

// #105: the fallback on both screens differs in its nouns and currency,
// so the shared skeleton is pinned. Read from ru.json: the question is
// whether two sentences agree, answerable only where they live.
describe("the fallback both screens fall back to", () => {
  const positions = ru.positions.notConverted;
  const operations = ru.operations.notConverted;

  it("says on both screens that the base-currency figures were withheld", () => {
    for (const sentence of [positions, operations]) {
      expect(sentence).toContain("В базовой валюте эта");
      expect(sentence).toContain("не посчиталась, а причина не названа.");
      expect(sentence).toContain("Поэтому числа этой строки показаны в");
    }
  });

  it("names no cause on either screen, least of all a rate", () => {
    // «Нет курса» names a rate on the path reached because the cause is
    // unknown; `undated_lot` is already about a date.
    for (const sentence of [positions, operations]) {
      expect(sentence).not.toContain("Нет курса");
      expect(sentence.toLowerCase()).not.toContain("курс");
      expect(sentence.toLowerCase()).not.toContain("дат");
    }
  });

  it("differs between the screens only in what each screen's row and currency are", () => {
    // Anything beyond the skeleton and the two nouns is a divergence one
    // screen made alone.
    const skeleton = (sentence: string) =>
      sentence
        .replace("позиция", "СТРОКА")
        .replace("операция", "СТРОКА")
        .replace("в исходной валюте", "В СВОЕЙ ВАЛЮТЕ")
        .replace("в валюте операции", "В СВОЕЙ ВАЛЮТЕ");
    expect(skeleton(positions)).toBe(skeleton(operations));
  });
});
