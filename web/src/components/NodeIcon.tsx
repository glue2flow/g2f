import { createContext, useContext } from "react";
import type { LucideIcon } from "lucide-react";
import {
  ArrowRightLeft,
  ArrowUpDown,
  CalendarClock,
  Clock,
  Code,
  CopyMinus,
  CornerDownRight,
  Database,
  FileSpreadsheet,
  Funnel,
  GitBranch,
  Globe,
  Hourglass,
  ListEnd,
  Merge,
  PencilLine,
  Play,
  RefreshCw,
  ScrollText,
  Sigma,
  Signpost,
  Split,
  TextCursorInput,
  TriangleAlert,
  WandSparkles,
  Webhook,
  Workflow,
} from "lucide-react";
import type { ModuleInfo, NodeDef } from "../types";

/** Installed modules by id, so icons can be looked up without repeating them per node. */
export const ModulesContext = createContext<Map<string, ModuleInfo>>(new Map());

/** Line icons of the built-in nodes. */
const builtinIcons: Record<string, LucideIcon> = {
  "trigger.manual": Play,
  "trigger.schedule": Clock,
  "trigger.webhook": Webhook,
  "trigger.call": CornerDownRight,
  "trigger.error": TriangleAlert,
  "core.http": Globe,
  "core.set": PencilLine,
  "core.if": GitBranch,
  "core.switch": Signpost,
  "core.merge": Merge,
  "core.code": Code,
  "core.wait": Hourglass,
  "core.transform": WandSparkles,
  "core.log": ScrollText,
  "flow.executeWorkflow": Workflow,
  "data.store": Database,
  "data.removeDuplicates": CopyMinus,
  "data.filter": Funnel,
  "data.sort": ArrowUpDown,
  "data.limit": ListEnd,
  "data.splitOut": Split,
  "data.aggregate": Sigma,
  "data.map": ArrowRightLeft,
  "data.editFields": TextCursorInput,
  "data.dateTime": CalendarClock,
  "data.csv": FileSpreadsheet,
};

/** Logic gates use the IEC gate symbols. */
const gateSymbols: Record<string, string> = {
  "logic.and": "&",
  "logic.or": "≥1",
  "logic.xor": "=1",
};

const fallbackColor = "#64748b";

interface Props {
  /** Node type definition; omit for module-level icons (plugin list, connections). */
  def?: NodeDef;
  /** The module, for module-level icons or when def is missing. */
  module?: ModuleInfo;
  size?: number;
  className?: string;
}

/**
 * The icon of a node or plugin:
 * - built-in nodes: a line icon on the node's color;
 * - plugins with an icon: the image on a white tile with a border in the plugin's color;
 * - plugins without one: the plugin's color with the first letter of the plugin's name.
 *
 * Plugin icons are data URIs shown with <img>, so SVG content can never run scripts.
 */
export function NodeIcon({ def, module, size = 28, className = "" }: Props) {
  const modules = useContext(ModulesContext);
  const mod = module ?? (def ? modules.get(def.module) : undefined);
  const color = def?.color ?? mod?.color ?? fallbackColor;
  const style = {
    width: size,
    height: size,
    "--node-color": color,
    "--node-icon-size": `${size}px`,
  } as React.CSSProperties;
  const cls = `node-icon ${className}`;

  if (def && def.module === "builtin") {
    const Icon = builtinIcons[def.type];
    const gate = gateSymbols[def.type];
    if (Icon || gate) {
      return (
        <span className={`${cls} node-icon-builtin`} style={style} aria-hidden>
          {Icon ? (
            <Icon size={Math.round(size * 0.6)} strokeWidth={2} />
          ) : (
            <span className="node-icon-gate">{gate}</span>
          )}
        </span>
      );
    }
  }

  const image = def?.icon || mod?.icon;
  if (image) {
    return (
      <span className={`${cls} node-icon-image`} style={style} aria-hidden>
        <img src={image} alt="" draggable={false} />
      </span>
    );
  }

  // No icon: the module's first letter on its color (built-in nodes: the node's).
  const label = (def?.module === "builtin" ? def.name : mod?.name || def?.category || def?.name || "?").trim();
  return (
    <span className={`${cls} node-icon-letter`} style={style} aria-hidden>
      {label.slice(0, 1).toUpperCase()}
    </span>
  );
}

/** Small badge for plugin triggers, whose icon is the plugin's logo. */
export function TriggerBadge({ def }: { def?: NodeDef }) {
  if (!def?.trigger || def.module === "builtin") return null;
  return (
    <span className="node-trigger-badge" title={def.trigger === "poll" ? "Polling trigger" : "Trigger"}>
      {def.trigger === "poll" ? <RefreshCw size={10} strokeWidth={3} /> : <Play size={10} strokeWidth={3} />}
    </span>
  );
}
