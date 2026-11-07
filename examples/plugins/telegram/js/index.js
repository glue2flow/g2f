// Telegram plugin for g8g, written in JavaScript and compiled to WebAssembly
// with the Extism JS PDK (QuickJS inside the wasm module):
//
//   extism-js index.js -i index.d.ts -o ../plugin.wasm
//
// Globals provided by the PDK: Host (input/output), Http (sandboxed requests
// to the manifest's allowedHosts), Config, Var. See index.d.ts for the export.

/** Entry point called by g8g once per node run. */
function execute() {
  const req = JSON.parse(Host.inputString());
  const items = req.items.map((it, i) => {
    try {
      return handle(req, it);
    } catch (e) {
      throw new Error(`item ${i}: ${e.message}`);
    }
  });
  Host.outputString(JSON.stringify({ items }));
}

function handle(req, it) {
  const p = it.params;
  switch (req.node) {
    case "sendMessage": {
      const payload = {
        chat_id: String(p.chatId ?? ""),
        text: String(p.text ?? ""),
        disable_notification: p.silent === true,
      };
      if (p.parseMode && p.parseMode !== "none") payload.parse_mode = p.parseMode;
      return call(req, it, "sendMessage", payload);
    }
    case "getMe":
      return call(req, it, "getMe", {});
    default:
      throw new Error(`unknown node "${req.node}"`);
  }
}

// The token comes from the node (per-user secret) or falls back to the
// organization's default (global config).
function call(req, it, method, payload) {
  const token = it.params.botToken || req.config.botToken;
  if (!token) throw new Error("no bot token: set one on the node or a default on the Plugins page");
  const base = String(req.config.apiBaseUrl || "https://api.telegram.org").replace(/\/+$/, "");

  const res = Http.request(
    { url: `${base}/bot${token}/${method}`, method: "POST", headers: { "Content-Type": "application/json" } },
    JSON.stringify(payload),
  );

  // Never put the URL (it contains the token) into error messages.
  let body;
  try {
    body = JSON.parse(res.body);
  } catch {
    throw new Error(`telegram: unexpected response (HTTP ${res.status})`);
  }
  if (!body.ok) throw new Error(`telegram: ${body.description} (code ${body.error_code})`);
  return body.result;
}

module.exports = { execute };
