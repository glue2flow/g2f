import { api } from "../api";

/**
 * Runs the OAuth2 flow for a plugin in a popup and resolves with the new
 * connection id. The callback page reports back with postMessage.
 */
export async function connectAccount(module: string, name?: string): Promise<string> {
  const { url } = await api.authorizeConnection(module, name);
  const popup = window.open(url, "g8g-oauth", "width=560,height=720");
  if (!popup) {
    window.location.href = url; // popup blocked: full-page redirect
    return new Promise(() => {});
  }
  return new Promise((resolve, reject) => {
    const onMessage = (e: MessageEvent) => {
      if (e.origin !== window.location.origin || e.data?.type !== "g8g-oauth") return;
      cleanup();
      if (e.data.ok) resolve(e.data.connectionId);
      else reject(new Error(e.data.error || "Connection failed"));
    };
    const timer = window.setInterval(() => {
      if (popup.closed) {
        cleanup();
        reject(new Error("The sign-in window was closed"));
      }
    }, 500);
    const cleanup = () => {
      window.removeEventListener("message", onMessage);
      window.clearInterval(timer);
    };
    window.addEventListener("message", onMessage);
  });
}
