import type { ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  Navigate,
  Outlet,
} from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useSession, useSetupStatus } from "@/api/session";
import { LoginPage } from "@/routes/login";
import { SetupPage } from "@/routes/setup";
import { AppLayout } from "@/routes/app-layout";
import { AccountsPage } from "@/routes/accounts";
import { AccountDetailPage } from "@/routes/accounts/detail";
import { FamilyPage } from "@/routes/family";
import { SettingsPage } from "@/routes/settings";

function FullScreenLoader() {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex items-center justify-center bg-background text-muted-foreground">
      {t("app.loading")}
    </div>
  );
}

// What the gate shows instead of a screen, when it has no screen it may
// honestly show. A message, and — where asking again can help — a button that
// asks again.
function StartupNotice({ message, onRetry }: { message: string; onRetry?: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex flex-col items-center justify-center gap-4 bg-background p-6 text-center">
      <p className="max-w-md text-sm text-muted-foreground">{message}</p>
      {onRetry && <Button onClick={onRetry}>{t("common.retry")}</Button>}
    </div>
  );
}

// Gate decides between setup wizard, login and the app shell.
//
// Two answers are needed for that: whether this instance has been set up, and
// whether this browser is signed in. Until both are in, the gate shows that it
// is waiting — it may not fall back to a default, because every default here is
// a claim about the server. `setup_needed ?? false` was one: it said "no setup
// needed" about an instance nobody had managed to ask, and a brand-new instance
// answered a login form for an account that does not exist yet, with nothing on
// screen to suggest the setup wizard was the screen wanted (#88).
//
// Exported for its tests; the routes below are its only other caller.
export function Gate({
  children,
  wants,
}: {
  children: ReactNode;
  wants: "app" | "login" | "setup";
}) {
  const { t } = useTranslation();
  const setupStatus = useSetupStatus();
  const session = useSession();

  // Nothing has answered yet — and the two ways of not having answered are not
  // the same news. "paused" is react-query holding a request because the
  // browser reports itself offline (networkMode "online", the default): it has
  // not been sent, no server has failed, and it will go out on its own when the
  // connection returns. It is also invisible to isLoading, which is
  // isPending && isFetching and therefore FALSE while paused — so the gate this
  // one replaced fell straight past it into the guess.
  if (setupStatus.isPending || session.isPending) {
    const paused =
      setupStatus.fetchStatus === "paused" || session.fetchStatus === "paused";
    return paused ? <StartupNotice message={t("app.startupOffline")} /> : <FullScreenLoader />;
  }

  // Read out rather than inferred from the checks above: react-query's result
  // is a union discriminated by `status`, and one `||` over two of them narrows
  // neither. Past the pending branch, an undefined here means the query failed
  // (useSetupStatus throws unless the body arrived), which is exactly what this
  // branch is for. A failed session query lands here too: "we could not ask
  // whether you are signed in" is not "you are not signed in", and answering it
  // with a login form would be the same guess in the other direction.
  const status = setupStatus.data;
  if (status === undefined || session.isError) {
    return (
      <StartupNotice
        message={t("app.startupUnknown")}
        onRetry={() => {
          void setupStatus.refetch();
          void session.refetch();
        }}
      />
    );
  }

  const setupNeeded = status.setup_needed;
  const authed = Boolean(session.data);

  if (setupNeeded && wants !== "setup") return <Navigate to="/setup" />;
  if (!setupNeeded && wants === "setup") return <Navigate to="/login" />;
  if (!authed && wants === "app") return <Navigate to="/login" />;
  if (authed && wants === "login") return <Navigate to="/" />;
  return <>{children}</>;
}

const rootRoute = createRootRoute({ component: () => <Outlet /> });

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: () => (
    <Gate wants="login">
      <LoginPage />
    </Gate>
  ),
});

const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: () => (
    <Gate wants="setup">
      <SetupPage />
    </Gate>
  ),
});

const layoutRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  component: () => (
    <Gate wants="app">
      <AppLayout />
    </Gate>
  ),
});

const indexRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/",
  component: () => <Navigate to="/accounts" />,
});

const accountsRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/accounts",
  component: AccountsPage,
});

const accountDetailRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/accounts/$accountId",
  component: AccountDetailPage,
});

const familyRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/family",
  component: FamilyPage,
});

const settingsRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings",
  component: SettingsPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute,
  layoutRoute.addChildren([
    indexRoute,
    accountsRoute,
    accountDetailRoute,
    familyRoute,
    settingsRoute,
  ]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
