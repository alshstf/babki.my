import type { ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  Navigate,
  Outlet,
} from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { useSession, useSetupStatus } from "@/api/session";
import { LoginPage } from "@/routes/login";
import { SetupPage } from "@/routes/setup";
import { AppLayout } from "@/routes/app-layout";
import { AccountsPage } from "@/routes/accounts";
import { AccountDetailPage } from "@/routes/accounts/detail";
import { FamilyPage } from "@/routes/family";

function FullScreenLoader() {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex items-center justify-center bg-background text-muted-foreground">
      {t("app.loading")}
    </div>
  );
}

// Gate decides between setup wizard, login and the app shell.
function Gate({
  children,
  wants,
}: {
  children: ReactNode;
  wants: "app" | "login" | "setup";
}) {
  const setupStatus = useSetupStatus();
  const session = useSession();

  if (setupStatus.isLoading || session.isLoading) return <FullScreenLoader />;

  const setupNeeded = setupStatus.data?.setup_needed ?? false;
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

const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute,
  layoutRoute.addChildren([indexRoute, accountsRoute, accountDetailRoute, familyRoute]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
