import { useState } from "react";
import type { Execution } from "../types";

interface Props {
  executions: Execution[];
  selected?: Execution;
  canCancel: boolean;
  onSelect: (id: string) => void;
  onRefresh: () => void;
  onCancel: (id: string) => void;
}

export function ExecutionsPanel({ executions, selected, canCancel, onSelect, onRefresh, onCancel }: Props) {
  const [stepId, setStepId] = useState<string>();
  const step = selected?.steps?.find((s) => s.nodeId === stepId) ?? selected?.steps?.find((s) => s.status === "error");

  return (
    <section className="exec-panel">
      <div className="exec-list">
        <div className="exec-list-head">
          <strong>Executions</strong>
          <button className="btn ghost small" onClick={onRefresh}>
            ↻
          </button>
        </div>
        {executions.length === 0 && <div className="muted small pad">No runs yet.</div>}
        {executions.map((e) => (
          <button key={e.id} className={`exec-row ${selected?.id === e.id ? "active" : ""}`} onClick={() => onSelect(e.id)}>
            <span className={`status-dot ${e.status}`} />
            <span className="exec-time">{new Date(e.startedAt).toLocaleTimeString()}</span>
            <span className="chip small">{e.triggerType}</span>
            <span className="muted small">{duration(e)}</span>
          </button>
        ))}
      </div>
      <div className="exec-detail">
        {!selected ? (
          <div className="muted pad">Select a run to inspect node outputs.</div>
        ) : (
          <>
            <div className="exec-detail-head">
              <span className={`chip st-${selected.status}`}>{selected.status}</span>
              <span className="muted small">
                {selected.triggerType} · by {selected.triggeredBy || "system"} · v{selected.workflowVersion}
              </span>
              {selected.error && <span className="error-text small">{selected.error}</span>}
              {selected.status === "waiting" && (
                <>
                  <span className="muted small">
                    {selected.waitMode === "webhook" ? "waiting for its resume URL" : "resumes"}
                    {selected.resumeAt &&
                      `${selected.waitMode === "webhook" ? ", times out" : ""} at ${new Date(selected.resumeAt).toLocaleString()}`}
                  </span>
                  {canCancel && (
                    <button className="btn small ghost danger" onClick={() => onCancel(selected.id)}>
                      Cancel
                    </button>
                  )}
                </>
              )}
            </div>
            <div className="steps">
              <div className="step-tabs">
                {selected.steps?.map((s) => (
                  <button
                    key={s.nodeId}
                    className={`step-tab st-${s.status} ${step?.nodeId === s.nodeId ? "active" : ""}`}
                    onClick={() => setStepId(s.nodeId)}
                  >
                    {s.nodeName}
                  </button>
                ))}
              </div>
              {step && (
                <div className="step-body">
                  <div className="muted small">
                    {step.status} · {step.inputCount} in · {step.durationMs} ms {step.truncated && "· output truncated"}
                  </div>
                  {step.error && <pre className="error-text">{step.error}</pre>}
                  {step.output && <pre className="json">{JSON.stringify(step.output, null, 2)}</pre>}
                </div>
              )}
            </div>
          </>
        )}
      </div>
    </section>
  );
}

function duration(e: Execution) {
  if (e.status === "waiting") return "waiting";
  if (!e.finishedAt) return e.status;
  const ms = new Date(e.finishedAt).getTime() - new Date(e.startedAt).getTime();
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}
