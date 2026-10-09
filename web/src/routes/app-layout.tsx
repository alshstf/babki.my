import { useState, type ReactNode } from "react";
import { Link, Outlet } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { KeyRound, LogOut, Settings, Users, Wallet } from "lucide-react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { BackgroundActivity } from "@/components/background-activity";
import { DisplayCurrencyToggle } from "@/components/display-currency-toggle";
import { useLogout, useSession } from "@/api/session";
import { PasswordDialog } from "@/components/password-dialog";
import { TouchTitles } from "@/components/touch-titles";
import {
  ScreenCurrencyCountProvider,
  useHasMultipleScreenCurrencies,
} from "@/lib/screen-currencies";

// HeaderCurrencyToggle shows the toggle only when the mounted screen has
// more than one currency; split out because it must be inside the provider
// AppLayout renders.
function HeaderCurrencyToggle() {
  const visible = useHasMultipleScreenCurrencies();
  return <DisplayCurrencyToggle visible={visible} />;
}

// One entry of the nav: icon and name side by side, the name shown from sm up
// and always the link's accessible name.
function NavLink({ to, icon, label }: { to: "/accounts" | "/family" | "/settings"; icon: ReactNode; label: string }) {
  return (
    <Link
      to={to}
      aria-label={label}
      title={label}
      className="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-accent [&.active]:bg-accent"
    >
      {icon} <span className="hidden sm:inline">{label}</span>
    </Link>
  );
}

export function AppLayout() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const logout = useLogout();
  const [passwordOpen, setPasswordOpen] = useState(false);

  return (
    // The provider wraps both the header (reader) and the Outlet (whose screen
    // reports).
    <ScreenCurrencyCountProvider>
      {/* A column beside the screen from md up; on a phone the same nav is a
          bar across the top, icons only, so the screen keeps the width. */}
      <div className="min-h-screen bg-background text-foreground flex flex-col md:flex-row">
        <aside className="flex items-center border-b md:w-56 md:flex-col md:items-stretch md:border-b-0 md:border-r">
          <div className="px-4 py-3 text-lg font-bold tracking-tight md:py-4">
            {t("app.name")}
          </div>
          <nav className="flex flex-1 gap-1 px-2 md:grid md:content-start">
            <NavLink to="/accounts" icon={<Wallet className="size-4" />} label={t("nav.accounts")} />
            <NavLink to="/family" icon={<Users className="size-4" />} label={t("nav.family")} />
            {session?.role === "owner" && (
              <NavLink to="/settings" icon={<Settings className="size-4" />} label={t("nav.settings")} />
            )}
          </nav>
        </aside>
        <div className="flex min-w-0 flex-1 flex-col">
          <header className="border-b px-4 py-3 flex items-center justify-end gap-3 md:px-6">
            {session && (
              <>
                {/* Who is signed in, from sm up: on a phone the row is the
                    currency toggle's and the sign-out's. */}
                <span className="hidden text-sm sm:inline">{session.user.display_name}</span>
                <Badge variant="secondary" className="hidden sm:inline-flex">
                  {t(`roles.${session.role}`)}
                </Badge>
                <BackgroundActivity />
                <HeaderCurrencyToggle />
                {/* Disabled only while a request is in flight: with networkMode
                   "always" isPending no longer covers a request held offline. */}
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={t("password.menu")}
                  title={t("password.menu")}
                  onClick={() => setPasswordOpen(true)}
                >
                  <KeyRound className="size-4" />
                </Button>
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
          {/* A failed sign-out must not be missed: the screen still looks signed
             in and the person may be walking away. Full width, until the next
             attempt answers. */}
          {logout.isError && (
            <Alert variant="destructive" className="rounded-none border-x-0 border-t-0">
              <AlertDescription>{t("auth.signOutFailed")}</AlertDescription>
            </Alert>
          )}
          <main className="min-w-0 flex-1 p-4 md:p-6">
            <Outlet />
          </main>
          <PasswordDialog open={passwordOpen} onOpenChange={setPasswordOpen} />
          <TouchTitles />
        </div>
      </div>
    </ScreenCurrencyCountProvider>
  );
}
