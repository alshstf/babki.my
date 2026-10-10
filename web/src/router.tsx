import type { ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Navigate,
  Outlet,
} from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { useSession, useSetupStatus } from "@/api/session";
import { AppLayout } from "@/routes/app-layout";

// Every screen is fetched on first visit (#15); one bundle had passed 660 kB,
// all of it needed before the login form. The shell and Gate are not split: they
// are on the way to every screen. lazyRouteComponent rather than React.lazy, so
// the router loads the chunk while matching; no link preloading. The second
// argument names the export.
const LoginPage = lazyRouteComponent(
  () => import("@/routes/login"),
  "LoginPage",
);
const SetupPage = lazyRouteComponent(
  () => import("@/routes/setup"),
  "SetupPage",
);
const AccountsPage = lazyRouteComponent(
  () => import("@/routes/accounts"),
  "AccountsPage",
);
const AccountDetailPage = lazyRouteComponent(
  () => import("@/routes/accounts/detail"),
  "AccountDetailPage",
);
const InstrumentPage = lazyRouteComponent(
  () => import("@/routes/instruments/detail"),
  "InstrumentPage",
);
const ImportPage = lazyRouteComponent(
  () => import("@/routes/accounts/import-page"),
  "ImportPage",
);
const FamilyPage = lazyRouteComponent(
  () => import("@/routes/family"),
  "FamilyPage",
);
const SettingsPage = lazyRouteComponent(
  () => import("@/routes/settings"),
  "SettingsPage",
);
const InstrumentsPage = lazyRouteComponent(
  () => import("@/routes/settings/instruments"),
  "InstrumentsPage",
);
const CategoriesPage = lazyRouteComponent(
  () => import("@/routes/settings/categories"),
  "CategoriesPage",
);
const ConnectWizardPage = lazyRouteComponent(
  () => import("@/routes/settings/connections/connect"),
  "ConnectWizardPage",
);
const ConnectionDetailPage = lazyRouteComponent(
  () => import("@/routes/settings/connections/detail"),
  "ConnectionDetailPage",
);

function FullScreenLoader() {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex items-center justify-center bg-background text-muted-foreground">
      {t("app.loading")}
    </div>
  );
}

// What the gate shows when it has no screen it may honestly show: a message,
// and a retry where asking again can help.
function StartupNotice({
  message,
  onRetry,
}: {
  message: string;
  onRetry?: () => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex flex-col items-center justify-center gap-4 bg-background p-6 text-center">
      <p className="max-w-md text-sm text-muted-foreground">{message}</p>
      {onRetry && <Button onClick={onRetry}>{t("common.retry")}</Button>}
    </div>
  );
}

// Gate decides between setup, login and the app. It needs whether the instance
// is set up and, only then, whether this browser is signed in, and shows that it
// is waiting until the needed answer is in: every default is a claim about the
// server (`setup_needed ?? false` showed a login form on a brand-new instance,
// #88). Exported for its tests.
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

  // The two questions in order: the first can make the second moot.

  // First: is the instance set up? "paused" is react-query holding the request
  // offline; it is invisible to isLoading, so this checks isPending.
  if (setupStatus.isPending) {
    return setupStatus.fetchStatus === "paused" ? (
      <StartupNotice message={t("app.startupOffline")} />
    ) : (
      <FullScreenLoader />
    );
  }

  // An undefined here means the query failed (useSetupStatus throws unless
  // a body arrived).
  const status = setupStatus.data;
  if (status === undefined) {
    return (
      <StartupNotice
        message={t("app.setupUnknown")}
        onRetry={() => {
          void setupStatus.refetch();
          void session.refetch();
        }}
      />
    );
  }

  // An instance not set up has one screen, the wizard; whether this browser
  // is signed in does not matter. /setup itself renders rather than
  // redirects.
  if (status.setup_needed) {
    return wants === "setup" ? <>{children}</> : <Navigate to="/setup" />;
  }

  // Second: is this browser signed in? Only now does it matter.
  if (session.isPending) {
    return session.fetchStatus === "paused" ? (
      <StartupNotice message={t("app.startupOffline")} />
    ) : (
      <FullScreenLoader />
    );
  }

  // "We could not ask" is not "not signed in". Only the session is retried.
  // isError alone also covers a failed refresh of an answer still cached
  // (data survives); data === undefined is "never answered", while null is
  // "nobody signed in". Without the second half, a background refresh failing
  // (a laptop waking, a server restarting) replaced the app with this
  // notice.
  if (session.isError && session.data === undefined) {
    return (
      <StartupNotice
        message={t("app.sessionUnknown")}
        onRetry={() => {
          void session.refetch();
        }}
      />
    );
  }

  const authed = Boolean(session.data);

  // The instance is set up, so the wizard has nothing to do.
  if (wants === "setup") return <Navigate to="/login" />;
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

// Loading a table of operations into one account.
const importRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/accounts/$accountId/import",
  component: ImportPage,
});

// One paper across the family's accounts.
const instrumentDetailRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/instruments/$instrumentId",
  component: InstrumentPage,
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

// The instrument catalog, the only place a row can be corrected; under
// settings because it is instance-wide.
const instrumentsRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/instruments",
  component: InstrumentsPage,
});

// The family's categories of spending and income. The settings page links
// here; the page itself also serves editors, who may change categories.
const categoriesRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/categories",
  component: CategoriesPage,
});

// "new" outranks the sibling $connectionId whatever the declaration order:
// TanStack Router ranks a literal segment above a param.
const connectWizardRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/connections/new",
  component: ConnectWizardPage,
});

// One connection's screen: its state and controls, accounts, last check,
// run log and unreadable operations (see ConnectionDetailPage).
const connectionDetailRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/connections/$connectionId",
  component: ConnectionDetailPage,
});

// Exported for the test that walks it: it must be this tree, since the
// check is that on-demand screens really arrive.
export const routeTree = rootRoute.addChildren([
  loginRoute,
  setupRoute,
  layoutRoute.addChildren([
    indexRoute,
    accountsRoute,
    accountDetailRoute,
    importRoute,
    instrumentDetailRoute,
    familyRoute,
    settingsRoute,
    instrumentsRoute,
    categoriesRoute,
    connectWizardRoute,
    connectionDetailRoute,
  ]),
]);

// What a route shows when its screen throws: typically a stale tab after an
// upgrade, whose screen chunk is gone (404) (#201). It reloads only on a click,
// so a reload that does not help cannot loop.
export function ScreenCrashed() {
  const { t } = useTranslation();
  return (
    <div
      className="min-h-screen flex flex-col items-center justify-center gap-4 bg-background p-6 text-center"
      data-testid="screen-crashed"
    >
      <p className="max-w-md text-sm text-muted-foreground">{t("app.crashed")}</p>
      <Button onClick={() => window.location.reload()}>{t("app.reload")}</Button>
    </div>
  );
}

export const router = createRouter({ routeTree, defaultErrorComponent: ScreenCrashed });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
