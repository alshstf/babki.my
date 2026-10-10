// babki.my keeps nothing of the family's on the device: the worker caches one
// page, the one that says the server cannot be reached, and answers a page
// load with it only when the network fails. Every other request goes to the
// network untouched, so an upgrade or a sign-out is never hidden by a cache.
const CACHE = "babki-offline-v1";
const OFFLINE = "/offline.html";

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((cache) => cache.addAll([OFFLINE, "/icon-192.png", "/favicon.svg"]))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const { request } = event;
  if (request.mode === "navigate") {
    event.respondWith(fetch(request).catch(() => caches.match(OFFLINE)));
    return;
  }
  // The offline page's own picture, when it is the offline page asking.
  if (request.url.endsWith("/icon-192.png") || request.url.endsWith("/favicon.svg")) {
    event.respondWith(fetch(request).catch(() => caches.match(request)));
  }
});

// Reminders pushed by the server (decision Р-27): the text was encrypted for
// this device; shown as it came, a tap opens the page it names.
self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch {
    data = { body: event.data ? event.data.text() : "" };
  }
  event.waitUntil(
    self.registration.showNotification(data.title || "babki.my", {
      body: data.body || "",
      tag: data.tag,
      icon: "/icon-192.png",
      badge: "/icon-192.png",
      data: { url: data.url || "/accounts" },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data?.url || "/accounts", self.location.origin).href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((tabs) => {
      for (const tab of tabs) {
        if (tab.url.startsWith(self.location.origin) && "focus" in tab) {
          tab.navigate(url);
          return tab.focus();
        }
      }
      return self.clients.openWindow(url);
    }),
  );
});
