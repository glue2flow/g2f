import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "@xyflow/react/dist/style.css";
import "./styles.css";
import { initAuth } from "./api";
import { App } from "./App";

const root = createRoot(document.getElementById("root")!);

initAuth()
  .then((cfg) =>
    root.render(
      <StrictMode>
        <App config={cfg} />
      </StrictMode>,
    ),
  )
  .catch((err) => {
    root.render(<div className="fatal">Could not start g8g: {String(err?.message ?? err)}</div>);
  });
