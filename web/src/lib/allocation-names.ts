// kindName is a kind's words; a switch with literal keys for
// scripts/check-i18n.mjs.
export function kindName(t: (key: string, v?: Record<string, unknown>) => string, key: string, base: string): string {
  switch (key) {
    case "shares":
      return t("allocation.kinds.shares");
    case "bonds":
      return t("allocation.kinds.bonds");
    case "funds":
      return t("allocation.kinds.funds");
    case "foreign_currency":
      return t("allocation.kinds.foreignCurrency");
    case "metals":
      return t("allocation.kinds.metals");
    case "deposits":
      return t("allocation.kinds.deposits");
    case "crypto":
      return t("allocation.kinds.crypto");
    case "broker_cash":
      return t("allocation.kinds.brokerCash", { currency: base });
    default:
      return t("allocation.kinds.other");
  }
}

// A currency's words: gold and the other metals held as money by name.
export function currencyName(t: (key: string) => string, key: string): string {
  switch (key) {
    case "XAU":
      return t("allocation.metalCodes.gold");
    case "XAG":
      return t("allocation.metalCodes.silver");
    default:
      return key;
  }
}
