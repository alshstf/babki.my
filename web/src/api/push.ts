import { api } from "./client";
import { apiError } from "./operations";

// The browser's push support, step by step: «unsupported» where there is no
// push at all (an iPhone's Safari outside the installed app), «denied» when
// the person refused notifications for the site, else whether this device
// has reminders on.
export type PushState = "unsupported" | "denied" | "off" | "on";

function supported(): boolean {
  return typeof window !== "undefined" && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
}

async function registration(): Promise<ServiceWorkerRegistration | null> {
  if (!supported()) return null;
  return (await navigator.serviceWorker.getRegistration()) ?? null;
}

export async function pushState(): Promise<PushState> {
  const reg = await registration();
  if (!reg) return "unsupported";
  if (Notification.permission === "denied") return "denied";
  return (await reg.pushManager.getSubscription()) ? "on" : "off";
}

// keyBytes turns the server's base64url key into what PushManager takes.
export function keyBytes(base64url: string): Uint8Array<ArrayBuffer> {
  const padded = base64url.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (base64url.length % 4)) % 4);
  const raw = atob(padded);
  const out = new Uint8Array(new ArrayBuffer(raw.length));
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

// turnOn asks the person, subscribes the device with the server's key, hands
// the address to the server and has it push a sample.
export async function turnOn(): Promise<PushState> {
  const reg = await registration();
  if (!reg) return "unsupported";
  const permission = await Notification.requestPermission();
  if (permission !== "granted") return permission === "denied" ? "denied" : "off";
  const key = await api.GET("/api/v1/push/key");
  if (!key.data) throw apiError(key.response, key.error);
  const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(key.data.public_key) });
  const json = sub.toJSON();
  const put = await api.PUT("/api/v1/push/subscriptions", {
    body: { endpoint: sub.endpoint, p256dh: json.keys?.p256dh ?? "", auth: json.keys?.auth ?? "" },
  });
  if (!put.response.ok) {
    await sub.unsubscribe();
    throw apiError(put.response, put.error);
  }
  await api.POST("/api/v1/push/test");
  return "on";
}

export async function turnOff(): Promise<PushState> {
  const reg = await registration();
  const sub = reg ? await reg.pushManager.getSubscription() : null;
  if (sub) {
    await api.DELETE("/api/v1/push/subscriptions", { params: { query: { endpoint: sub.endpoint } } });
    await sub.unsubscribe();
  }
  return reg ? "off" : "unsupported";
}
