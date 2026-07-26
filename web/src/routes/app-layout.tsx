import { Link, Outlet } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { LogOut, Users, Wallet } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { useLogout, useSession } from "@/api/session";

export function AppLayout() {
  const { t } = useTranslation();
  const { data: session } = useSession();
  const logout = useLogout();

  return (
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
        </nav>
      </aside>
      <div className="flex-1 flex flex-col">
        <header className="border-b px-6 py-3 flex items-center justify-end gap-3">
          {session && (
            <>
              <span className="text-sm">{session.user.display_name}</span>
              <Badge variant="secondary">{t(`roles.${session.role}`)}</Badge>
              <Button
                variant="ghost"
                size="icon"
                aria-label={t("auth.signOut")}
                onClick={() => logout.mutate()}
              >
                <LogOut className="size-4" />
              </Button>
            </>
          )}
        </header>
        <main className="flex-1 p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
