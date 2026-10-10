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
