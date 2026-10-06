import { expect, test } from "@playwright/test";

// The owner's first path through a fresh instance with the demo data: sign
// in, open a brokerage account, buy ten shares, and see the position grow by
// ten. It runs the built page against the real server and database.
test("sign in, buy shares, see the position grow", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("Логин").fill("demo");
  await page.getByLabel("Пароль").fill("demo1234");
  await page.getByRole("button", { name: "Войти" }).click();

  await expect(page.getByText("Итого").first()).toBeVisible();
  await page.getByRole("link", { name: "Брокерский Т-Банк" }).click();

  const sberQuantity = page
    .getByRole("row")
    .filter({ hasText: "SBER" })
    .getByRole("cell")
    .nth(1);
  await expect(sberQuantity).toBeVisible();
  const before = Number(
    (await sberQuantity.innerText()).replace(/\s/g, "").replace(",", "."),
  );

  await page.getByRole("button", { name: /Добавить операцию/ }).click();
  await page.getByRole("menuitem", { name: "Покупка" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("Поиск инструмента").fill("SBER");
  await dialog.getByRole("button", { name: /Сбербанк/ }).click();
  await dialog.getByLabel("Количество").fill("10");
  await dialog.getByLabel(/Цена за единицу/).fill("300");
  await dialog.getByRole("button", { name: "Покупка" }).click();
  await expect(dialog).toBeHidden();

  await expect(sberQuantity).toHaveText(
    String(before + 10).replace(/\B(?=(\d{3})+(?!\d))/g, " "),
    {
      useInnerText: true,
    },
  );
});
