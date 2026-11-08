import { useState } from "react";
import type { Execution, LogEntry } from "../types";

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
  const logs = (selected?.steps ?? []).flatMap((s) => (s.logs ?? []).map((l) => ({ ...l, nodeName: s.nodeName })));
  // The run log is the default view when there is one, unless a node failed.
  const errorStep = selected?.steps?.find((s) => s.status === "error");
  const showLog = logs.length > 0 && (stepId === LOG_TAB || (!stepId && !errorStep));
  const step = showLog ? undefined : (selected?.steps?.find((s) => s.nodeId === stepId) ?? errorStep);

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
              {!!selected.attempts && (
                <span
                  className="chip small"
                  title="The server running this execution stopped; another one continued after the last completed node."
                >
                  ↻ continued {selected.attempts === 1 ? "once" : `${selected.attempts}×`} after a server stop
                </span>
              )}
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
                {logs.length > 0 && (
                  <button className={`step-tab log-tab ${showLog ? "active" : ""}`} onClick={() => setStepId(LOG_TAB)}>
                    Log <span className="count">{logs.length}</span>
                  </button>
                )}
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
              {showLog && (
                <div className="step-body">
                  <LogList entries={logs} />
                </div>
              )}
              {step && (
                <div className="step-body">
                  <div className="muted small">
                    {step.status} · {step.inputCount} in · {step.durationMs} ms {step.truncated && "· output truncated"}
                  </div>
                  {step.error && <pre className="error-text">{step.error}</pre>}
                  {step.logs && step.logs.length > 0 && <LogList entries={step.logs} />}
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

const LOG_TAB = "\u0000log";

function LogList({ entries }: { entries: (LogEntry & { nodeName?: string })[] }) {
  return (
    <ol className="log-list">
      {entries.map((l, i) => (
        <li key={i} className={`log-entry lv-${l.level}`}>
          <time className="log-time" dateTime={l.time} title={new Date(l.time).toLocaleString()}>
            {new Date(l.time).toLocaleTimeString([], { hour12: false })}
          </time>
          <span className="log-level">{l.level}</span>
          {l.nodeName && <span className="log-node">{l.nodeName}</span>}
          <pre className="log-msg">{l.message}</pre>
        </li>
      ))}
    </ol>
  );
}

function duration(e: Execution) {
  if (e.status === "waiting") return "waiting";
  if (!e.finishedAt) return e.status;
  const ms = new Date(e.finishedAt).getTime() - new Date(e.startedAt).getTime();
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}
