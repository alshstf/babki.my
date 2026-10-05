// The currency codes offered as ready-made choices wherever a currency is
// picked: the base currency (routes/settings) and an account's own
// (routes/accounts/account-dialog). Both also offer «другая», so the list is a
// shortcut, never a limit. One list for both: a code offered for an account
// but not for the base currency would leave money that cannot be totalled
// (#33). The rouble leads because every stored rate is quoted against it.
export const COMMON_CURRENCIES = ["RUB", "USD", "EUR", "KZT"];

// The shape the server accepts for a currency code: currency.Pattern in Go,
// whose test holds this literal to it.
const CURRENCY_CODE = /^[A-Z]{3}$/;

// isCurrencyCode is whether a form may send code as a currency.
export function isCurrencyCode(code: string): boolean {
  return CURRENCY_CODE.test(code);
}
