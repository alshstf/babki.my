import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import "./i18n";
import "./index.css";
import { router } from "./router";

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, refetchOnWindowFocus: false },
    // A mutation is sent whatever the browser believes about the network: a
    // paused one leaves its button disabled with no error and no end, and goes
    // out on its own some unknown time later (#200). Sent at once, it either
    // succeeds or fails where the person can see it.
    mutations: { networkMode: "always" },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);

// Installed on a phone, the app needs a worker to say «нет связи» rather than
// the browser's error page (public/sw.js). Not in development: Vite serves
// its own files and a worker would only get in its way.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {
      // Without the worker the app works the same, only offline it shows the browser's own page.
    });
  });
}
