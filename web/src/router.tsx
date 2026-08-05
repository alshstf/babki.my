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
import { ConnectWizardPage } from "@/routes/settings/connections/connect";
import { ConnectionDetailPage } from "@/routes/settings/connections/detail";

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
// Up to two answers are needed for that: whether this instance has been set up
// and — only if it has been — whether this browser is signed in. Until the
// answer it actually needs is in, the gate shows that it is waiting; it may not
// fall back to a default, because every default here is a claim about the
// server. `setup_needed ?? false` was one: it said "no setup
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

  // The two questions are asked in order, because the answer to the first can
  // make the second beside the point — and a gate that waits for, or gives up
  // over, an answer it does not need is guessing in its own way: it says it does
  // not know when it does.

  // FIRST: has this instance been set up? Until that is in, no screen here can
  // be shown honestly. "paused" is react-query holding the request because the
  // browser reports itself offline (networkMode "online", the default): it has
  // not been sent, no server has failed, and it will go out on its own when the
  // connection returns. It is also invisible to isLoading, which is
  // isPending && isFetching and therefore FALSE while paused — so the gate this
  // one replaced fell straight past it into the guess.
  if (setupStatus.isPending) {
    return setupStatus.fetchStatus === "paused" ? (
      <StartupNotice message={t("app.startupOffline")} />
    ) : (
      <FullScreenLoader />
    );
  }

  // Read out rather than inferred from the check above: react-query's result is
  // a union discriminated by `status`, and the pending check narrows it only
  // for this exact expression. Past it, an undefined means the query failed
  // (useSetupStatus throws unless the body arrived).
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

  // An instance nobody has set up yet has exactly one screen it can show: the
  // wizard. There is no account to sign in to, so whether this browser is
  // signed in is a question with no bearing — and the gate must not stall, or
  // give up, over the answer to it. Reaching /setup ITSELF is the one case that
  // renders rather than redirects.
  if (status.setup_needed) {
    return wants === "setup" ? <>{children}</> : <Navigate to="/setup" />;
  }

  // SECOND: is this browser signed in? Only now does it matter — and only now
  // may not knowing stop anything.
  if (session.isPending) {
    return session.fetchStatus === "paused" ? (
      <StartupNotice message={t("app.startupOffline")} />
    ) : (
      <FullScreenLoader />
    );
  }

  // "We could not ask whether you are signed in" is not "you are not signed
  // in", and answering it with a login form would be the same guess in the
  // other direction. Only the session is asked again: the setup status has
  // already answered.
  //
  // `isError` alone is not "we never learned the answer" — react-query sets
  // it whenever the LAST attempt failed, even when an earlier one already
  // succeeded and the cache still holds it (`session.data` survives a failed
  // refetch untouched). `data === undefined` is what "never answered" looks
  // like; `null` already means "answered: nobody is signed in". Without the
  // second half, a signed-in reader's background refresh failing — the
  // laptop waking from sleep, `online` firing while the server is still
  // coming up, or the owner restarting the docker stand with the tab open —
  // replaced the whole app with this notice, and the notice would have been
  // false: the client did know a moment ago and still holds the answer, it
  // just failed to refresh it. This was a regression: the guard this
  // replaced, `isLoading`, is `isPending && isFetching` and so goes false
  // once data exists, which let a failed refresh fall through and render
  // from cache correctly, by accident, on the base commit.
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

  // Past the branch above the instance IS set up, so the wizard has nothing
  // left to do here whoever is asking for it.
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

// Static segments ("new") take precedence over the sibling dynamic one
// ($connectionId) regardless of declaration order — TanStack Router ranks a
// literal path segment above a param segment when matching.
const connectWizardRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/connections/new",
  component: ConnectWizardPage,
});

// Stub for now — see ConnectionDetailPage. A later task fills this in with
// the run log, the reconcile snapshot and the unparsed-operations list.
const connectionDetailRoute = createRoute({
  getParentRoute: () => layoutRoute,
  path: "/settings/connections/$connectionId",
  component: ConnectionDetailPage,
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
    connectWizardRoute,
    connectionDetailRoute,
  ]),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
