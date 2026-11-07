import { Handle, Position, type Node, type NodeProps } from "@xyflow/react";
import type { NodeDef, Step } from "../types";

export interface FlowNodeData extends Record<string, unknown> {
  type: string;
  name: string;
  params: Record<string, unknown>;
  disabled?: boolean;
  def?: NodeDef;
  status?: Step["status"];
  itemCount?: number;
}

export type FlowNodeType = Node<FlowNodeData, "g8g">;

const triggerIcon: Record<string, string> = { manual: "▶", schedule: "⏱", webhook: "⚡" };

export function FlowNode({ data, selected }: NodeProps<FlowNodeType>) {
  const def = data.def;
  const inputs = def?.inputs ?? ["main"];
  const outputs = def?.outputs ?? ["main"];
  const color = def?.color ?? "#64748b";
  const cls = ["flow-node", selected && "selected", data.disabled && "disabled", def?.trigger && "trigger", data.status && `st-${data.status}`]
    .filter(Boolean)
    .join(" ");

  return (
    <div className={cls} style={{ "--node-color": color } as React.CSSProperties}>
      {inputs.map((h, i) => (
        <Handle
          key={h}
          id={h}
          type="target"
          position={Position.Left}
          style={{ top: `${((i + 1) / (inputs.length + 1)) * 100}%` }}
        />
      ))}
      <div className="flow-node-icon">{def?.trigger ? triggerIcon[def.trigger] : def?.name.slice(0, 1)}</div>
      <div className="flow-node-body">
        <div className="flow-node-name">{data.name}</div>
        <div className="flow-node-type">{def?.name ?? `unknown: ${data.type}`}</div>
      </div>
      {data.status && (
        <div className="flow-node-status" title={data.status}>
          {data.status === "success" ? `✓ ${data.itemCount ?? 0}` : data.status === "error" ? "✕" : "–"}
        </div>
      )}
      {outputs.map((h, i) => {
        const top = `${((i + 1) / (outputs.length + 1)) * 100}%`;
        return (
          <div key={h}>
            <Handle id={h} type="source" position={Position.Right} style={{ top }} />
            {outputs.length > 1 && (
              <span className="handle-label" style={{ top }}>
                {h}
              </span>
            )}
          </div>
        );
      })}
    </div>
  );
}
