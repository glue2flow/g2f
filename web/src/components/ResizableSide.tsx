import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";

const STORAGE_KEY = "g2f.sidePanel.width";
const DEFAULT_WIDTH = 340;
const MIN_WIDTH = 280;
// Leave at least this much room for the canvas.
const MIN_CANVAS = 320;

function maxWidth() {
  return Math.max(MIN_WIDTH, window.innerWidth - MIN_CANVAS);
}

function clamp(w: number) {
  return Math.round(Math.min(Math.max(w, MIN_WIDTH), maxWidth()));
}

function loadWidth() {
  try {
    const w = Number(localStorage.getItem(STORAGE_KEY));
    return w > 0 ? clamp(w) : DEFAULT_WIDTH;
  } catch {
    return DEFAULT_WIDTH;
  }
}

function saveWidth(w: number) {
  try {
    localStorage.setItem(STORAGE_KEY, String(w));
  } catch {
    // storage unavailable: the width just isn't remembered
  }
}

/** Right-hand side panel with a drag handle on its left edge. The width is remembered per browser. */
export function ResizableSide({ children }: { children: ReactNode }) {
  const [width, setWidth] = useState(loadWidth);
  const [dragging, setDragging] = useState(false);
  const start = useRef({ x: 0, width: 0 });
  const latest = useRef(width);
  latest.current = width;

  // Keep the panel within bounds when the window shrinks.
  useEffect(() => {
    const onResize = () => setWidth((w) => clamp(w));
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);

  useEffect(() => {
    if (!dragging) return;
    const { cursor, userSelect } = document.body.style;
    document.body.style.cursor = "col-resize";
    document.body.style.userSelect = "none";
    return () => {
      document.body.style.cursor = cursor;
      document.body.style.userSelect = userSelect;
    };
  }, [dragging]);

  const set = (w: number) => {
    const c = clamp(w);
    setWidth(c);
    saveWidth(c);
  };

  return (
    <div className="side-wrap" style={{ "--side-width": `${width}px` } as CSSProperties}>
      <div
        className={`resize-grabber ${dragging ? "dragging" : ""}`}
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize panel"
        aria-valuenow={width}
        aria-valuemin={MIN_WIDTH}
        aria-valuemax={maxWidth()}
        tabIndex={0}
        title="Drag to resize, double-click to reset"
        onPointerDown={(e) => {
          if (e.button !== 0) return;
          e.preventDefault();
          e.currentTarget.setPointerCapture(e.pointerId);
          start.current = { x: e.clientX, width: latest.current };
          setDragging(true);
        }}
        onPointerMove={(e) => {
          if (!dragging) return;
          setWidth(clamp(start.current.width + start.current.x - e.clientX));
        }}
        onPointerUp={(e) => {
          if (!dragging) return;
          e.currentTarget.releasePointerCapture(e.pointerId);
          setDragging(false);
          saveWidth(latest.current);
        }}
        onPointerCancel={() => setDragging(false)}
        onDoubleClick={() => set(DEFAULT_WIDTH)}
        onKeyDown={(e) => {
          const step = e.shiftKey ? 64 : 16;
          if (e.key === "ArrowLeft") set(width + step);
          else if (e.key === "ArrowRight") set(width - step);
          else if (e.key === "Home") set(DEFAULT_WIDTH);
          else return;
          e.preventDefault();
        }}
      />
      {children}
    </div>
  );
}
