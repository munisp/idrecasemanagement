// Author: Patrick Munis at newwave
// The service worker is retired (2026-10-24). Browsers that installed the old one still fetch this file when they
// check for updates: it clears the portal's caches, unregisters itself and reloads its pages once, so they load
// fresh from the network. Nothing registers a service worker any more (js/app.js).
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    const keys = await caches.keys();
    await Promise.all(keys.filter((k) => k.startsWith("idre-")).map((k) => caches.delete(k)));
    await self.registration.unregister();
    const pages = await self.clients.matchAll({ type: "window" });
    pages.forEach((c) => c.navigate(c.url));
  })());
});
