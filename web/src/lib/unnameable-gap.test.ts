import { describe, expect, it } from "vitest";
import ru from "@/i18n/ru.json";
import { unnameableGap } from "./unnameable-gap";

describe("unnameableGap", () => {
  it("hands back the caller's fallback and invents nothing", () => {
    // The whole of the function, and the whole of what it is allowed to do: a
    // value off the wire that this build's union does not contain must reach
    // the screen as the caller's own sentence, never as a cause guessed here.
    // The cast is what a client one release behind the server actually gets —
    // these values are JSON typed by assertion, not validated.
    expect(unnameableGap("no_rate_next_tuesday" as never, "запасная фраза")).toBe("запасная фраза");
  });
});

// #105. The sentence a row falls back to when nobody can name its cause exists
// on both screens, and the two must keep saying the same thing. They cannot be
// the same STRING — one is about a position and one about an operation, and
// each names its own screen's native currency, exactly as the four named
// causes already do — so what is pinned here is the skeleton they share.
//
// Read out of ru.json rather than spelled out, unlike every caption assertion
// in the two table tests: those ask WHICH sentence a cell gets, and reading
// the sentence through the component's own lookup would agree with the
// component whichever it picked. This one asks whether TWO sentences still
// agree with each other, and that question has no answer at all unless both
// are read from where they live.
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
    // The defect itself. The sentence used to open «Нет курса» on both
    // screens, which asserts the cause is a missing RATE — on the one path
    // that is reached precisely because the cause is unknown. `undated_lot` is
    // in today's own enum and is about a DATE nobody wrote down; the next
    // date-shaped cause the server adds would be captioned «нет курса» by
    // every client compiled before it.
    for (const sentence of [positions, operations]) {
      expect(sentence).not.toContain("Нет курса");
      expect(sentence.toLowerCase()).not.toContain("курс");
      expect(sentence.toLowerCase()).not.toContain("дат");
    }
  });

  it("differs between the screens only in what each screen's row and currency are", () => {
    // The drift guard. Anything either sentence says beyond the shared
    // skeleton and its own two nouns is a divergence one screen made alone,
    // and the reader meets both tables on one account page.
    const skeleton = (sentence: string) =>
      sentence
        .replace("позиция", "СТРОКА")
        .replace("операция", "СТРОКА")
        .replace("в исходной валюте", "В СВОЕЙ ВАЛЮТЕ")
        .replace("в валюте операции", "В СВОЕЙ ВАЛЮТЕ");
    expect(skeleton(positions)).toBe(skeleton(operations));
  });
});
