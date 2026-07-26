import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from "@tanstack/react-router";
import { useTranslation } from "react-i18next";

const rootRoute = createRootRoute({
  component: () => <Outlet />,
});

function Stub() {
  const { t } = useTranslation();
  return (
    <div className="min-h-screen flex items-center justify-center bg-background text-foreground">
      {t("todo.stub")}
    </div>
  );
}

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  component: Stub,
});
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  component: Stub,
});
const setupRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/setup",
  component: Stub,
});

const routeTree = rootRoute.addChildren([indexRoute, loginRoute, setupRoute]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
