import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import "@/i18n";
import { AmountField, OperationDateField } from "./form-fields";
import { EARLIEST_OPERATION_DATE, localToday } from "@/lib/dates";

function amount(value: string, accepted: boolean) {
  return render(
    <AmountField
      id="a"
      label="Сумма, RUB"
      value={value}
      onChange={() => {}}
      currency="RUB"
      accepted={accepted}
      badNumber="Введите положительную сумму"
      hint={<p>подсказка</p>}
    />,
  );
}

describe("AmountField", () => {
  it("is named by its label and shows its hint", () => {
    amount("", false);
    expect(screen.getByRole("textbox", { name: "Сумма, RUB" })).toBeInTheDocument();
    expect(screen.getByText("подсказка")).toBeInTheDocument();
  });

  it("says nothing about an empty box or one the caller takes", () => {
    amount("", false);
    expect(screen.queryByText("Введите положительную сумму")).toBeNull();
    amount("100", true);
    expect(screen.queryByText("Введите положительную сумму")).toBeNull();
  });

  it("names a sum past the bound as too large, anything else in the caller's words", () => {
    amount("abc", false);
    expect(screen.getByText("Введите положительную сумму")).toBeInTheDocument();
    amount("99999999999999999999", false);
    expect(screen.getByText(/больше/i)).toBeInTheDocument();
  });
});

describe("OperationDateField", () => {
  it("offers the days the journal accepts", () => {
    render(<OperationDateField id="d" label="Дата" value="" onChange={() => {}} />);
    const field = screen.getByLabelText("Дата");
    expect(field).toHaveAttribute("min", EARLIEST_OPERATION_DATE);
    expect(field).toHaveAttribute("max", localToday());
  });
});
