import { Link, Outlet } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { LogOut, Settings, Users, Wallet } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DisplayCurrencyToggle } from "@/components/display-currency-toggle";
import { useLogout, useSession } from "@/api/session";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";

// Reads the count reported by whichever screen is mounted in <Outlet/>
// below (via useReportScreenCurrencies) and hides the toggle unless that
// screen actually has more than one currency in play — there's nothing for
// "native" vs "base" to differ on otherwise. Split out from AppLayout
// itself because it must be a *descendant* of ScreenCurrencyCountProvider
// to read the context; AppLayout renders that provider, so it can't also
// consume it in the same render pass.
function HeaderCurrencyToggle() {
  const visible = useHasMultipleScreenCurrencies();
  return <DisplayCurrencyToggle visible={visible} />;
}

export function AppLayout() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const logout = useLogout();

  return (
    // The screen-currency-count provider must wrap both the header (which
    // reads it, via HeaderCurrencyToggle) and <Outlet/> (whose mounted
    // screen writes it, via useReportScreenCurrencies) — it's the shared
    // ancestor connecting the two without any direct coupling between them.
    <ScreenCurrencyCountProvider>
      <div className="min-h-screen bg-background text-foreground flex">
        <aside className="w-56 border-r flex flex-col">
          <div className="px-4 py-4 text-lg font-bold tracking-tight">
            {t("app.name")}
          </div>
          <nav className="flex-1 px-2 grid gap-1 content-start">
            <Link
              to="/accounts"
              className="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-accent [&.active]:bg-accent"
            >
              <Wallet className="size-4" /> {t("nav.accounts")}
            </Link>
            <Link
              to="/family"
              className="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-accent [&.active]:bg-accent"
            >
              <Users className="size-4" /> {t("nav.family")}
            </Link>
            {session?.role === "owner" && (
              <Link
                to="/settings"
                className="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-accent [&.active]:bg-accent"
              >
                <Settings className="size-4" /> {t("nav.settings")}
              </Link>
            )}
          </nav>
        </aside>
        <div className="flex-1 flex flex-col">
          <header className="border-b px-6 py-3 flex items-center justify-end gap-3">
            {session && (
              <>
                <span className="text-sm">{session.user.display_name}</span>
                <Badge variant="secondary">{t(`roles.${session.role}`)}</Badge>
                <HeaderCurrencyToggle />
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={t("auth.signOut")}
                  onClick={() => logout.mutate()}
                  disabled={logout.isPending}
                >
                  <LogOut className="size-4" />
                </Button>
              </>
            )}
          </header>
          {/* A sign-out that did not go through is the one failure in this
              application a reader must not miss: it is invisible in every other
              way — the screen looks signed in, which is exactly what it is —
              and the person it misleads is already walking away from the
              machine. Full width under the header rather than a hint beside the
              button, and it stays until the next attempt answers. */}
          {logout.isError && (
            <Alert variant="destructive" className="rounded-none border-x-0 border-t-0">
              <AlertDescription>{t("auth.signOutFailed")}</AlertDescription>
            </Alert>
          )}
          <main className="flex-1 p-6">
            <Outlet />
          </main>
        </div>
      </div>
    </ScreenCurrencyCountProvider>
  );
}
