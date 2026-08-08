// The currency codes offered as ready-made choices wherever a currency is
// picked: the space's base currency (routes/settings) and an account's own
// (routes/accounts/account-dialog). Both selectors also offer «другая», which
// takes any three-letter code, so this list is a shortcut and never a limit —
// nothing here or on the server restricts a currency to these four.
//
// One list rather than one per screen, because the two are not independently
// chosen sets that happen to coincide: they answer the same question about the
// same user, and a code added for an account but not for the base currency
// would leave a person able to hold money they cannot total in (#33).
//
// Order is what the dropdown shows, so it is kept rather than sorted: the
// rouble leads because it is the currency every stored rate is quoted against
// (marketdata's quoteCurrency), and the rest follow in the order both screens
// have always offered them.
export const COMMON_CURRENCIES = ["RUB", "USD", "EUR", "KZT"];
