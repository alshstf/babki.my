import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useAccounts, type AccountWithBalance } from "@/api/accounts";
import { CashDialog } from "@/routes/accounts/cash-dialog";

// The account the last quick entry went to, in this browser only: a
// convenience, so a failure to read or write it changes nothing else.
const LAST_ACCOUNT = "babki.quickAdd.account";

// Where a family spends and earns day to day, in the order the list offers
// them: a brokerage account's money is investing, a loan's is paid from its
// own page.
const EVERYDAY: AccountWithBalance["type"][] = ["credit_card", "checking", "cash", "savings", "deposit"];

// The installed app's shortcut «Добавить операцию» starts it at /?add. Read
// once, as the bundle loads, before the router's redirect drops the query.
const startedToAdd = typeof window !== "undefined" && new URLSearchParams(window.location.search).has("add");

function lastAccount(): string | null {
  try {
    return localStorage.getItem(LAST_ACCOUNT);
  } catch {
    return null;
  }
}

function rememberAccount(id: string) {
  try {
    localStorage.setItem(LAST_ACCOUNT, id);
  } catch {
    // Remembering is a convenience: without storage the list opens on its first account.
  }
}

// QuickAdd is the spending (or earning) entered from any screen: the last
// account used, today's date, the operation dialog everyone already knows. A
// button in the header from md up, a round one under the thumb on a phone.
export function QuickAdd() {
  const { t } = useTranslation();
  const accounts = useAccounts();
  const [open, setOpen] = useState(startedToAdd);
  const everyday = (accounts.data ?? [])
    .filter((a) => a.status === "active" && EVERYDAY.includes(a.type))
    .sort((a, b) => EVERYDAY.indexOf(a.type) - EVERYDAY.indexOf(b.type));
  const remembered = lastAccount();
  const account = everyday.find((a) => a.id === remembered) ?? everyday[0];
  if (!account) return null;
  return (
    <>
      <Button size="sm" className="mr-auto hidden md:inline-flex" onClick={() => setOpen(true)}>
        <Plus className="size-4" />
        {t("quickAdd.button")}
      </Button>
      <Button
        size="icon"
        className="fixed right-4 bottom-4 z-40 size-14 rounded-full shadow-lg md:hidden"
        aria-label={t("quickAdd.button")}
        onClick={() => setOpen(true)}
      >
        <Plus className="size-6" />
      </Button>
      <CashDialog
        open={open}
        onOpenChange={setOpen}
        account={account}
        accounts={everyday}
        preset="expense"
        onSaved={(op) => rememberAccount(op.account_id)}
      />
    </>
  );
}
