// The words for each sector of a paper (decision Р-46); a switch with literal
// keys for scripts/check-i18n.mjs.
export function sectorName(t: (key: string) => string, key: string): string {
  switch (key) {
    case "financials":
      return t("structure.sectors.financials");
    case "energy":
      return t("structure.sectors.energy");
    case "it":
      return t("structure.sectors.it");
    case "consumer":
      return t("structure.sectors.consumer");
    case "telecom":
      return t("structure.sectors.telecom");
    case "utilities":
      return t("structure.sectors.utilities");
    case "materials":
      return t("structure.sectors.materials");
    case "industrials":
      return t("structure.sectors.industrials");
    case "health_care":
      return t("structure.sectors.healthCare");
    case "real_estate":
      return t("structure.sectors.realEstate");
    case "government":
      return t("structure.sectors.government");
    case "municipal":
      return t("structure.sectors.municipal");
    case "funds_equity":
      return t("structure.sectors.fundsEquity");
    case "funds_bonds":
      return t("structure.sectors.fundsBonds");
    case "funds_money":
      return t("structure.sectors.fundsMoney");
    case "funds_gold":
      return t("structure.sectors.fundsGold");
    case "funds_mixed":
      return t("structure.sectors.fundsMixed");
    default:
      return t("structure.sectors.unknown");
  }
}
