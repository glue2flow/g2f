import type { ClientConfig, Workflow } from "../types";
import type { FlowNodeData } from "./FlowNode";

interface Props {
  workflow: Workflow;
  nodeId: string;
  data: FlowNodeData;
  config: ClientConfig;
  dirty: boolean;
}

/** Shows trigger-specific hints (how the selected trigger is invoked). */
export function TriggerInfo({ data }: Props) {
  if (data.def?.trigger === "manual") {
    return <div className="info-box">Select this node and click ▶ Run to start the workflow from here.</div>;
  }
  return null;
}
