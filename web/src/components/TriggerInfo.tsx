import { useEffect, useState } from "react";
import { api } from "../api";
import type { ClientConfig, TriggerInfo as Info, Workflow } from "../types";
import type { FlowNodeData } from "./FlowNode";

interface Props {
  workflow: Workflow;
  nodeId: string;
  data: FlowNodeData;
  config: ClientConfig;
  dirty: boolean;
}

/** Explains how the selected trigger is invoked: webhook URL, next schedule runs. */
export function TriggerInfo({ workflow, nodeId, data, dirty }: Props) {
  const kind = data.def?.trigger;
  const [info, setInfo] = useState<Info>();

  useEffect(() => {
    if (kind !== "webhook" && kind !== "form" && kind !== "schedule" && kind !== "poll") return;
    api
      .triggers(workflow.id)
      .then((list) => setInfo(list.find((t) => t.nodeId === nodeId)))
      .catch(() => setInfo(undefined));
    // Refetch whenever a new version is saved.
  }, [workflow.id, workflow.version, workflow.active, nodeId, kind]);

  if (data.type === "core.wait" && (data.params.mode ?? "duration") === "webhook") {
    return (
      <div className="info-box">
        The execution pauses here until this node's resume URL is called (any HTTP method). Put the URL into an earlier node,
        e.g. an approval message: <code>{"{{ execution.resumeUrl }}?decision=approve"}</code> (only if the workflow has a single Wait
        node), otherwise <code>{"{{ execution.resumeUrls['Node name'] }}"}</code>. Each link resumes only its own Wait node. The
        request (query, body) is the output.
      </div>
    );
  }
  if (data.type === "core.wait") {
    return <div className="info-box">Paused executions are stored in the database and continue after restarts.</div>;
  }
  if (kind === "call") {
    return (
      <div className="info-box">
        Runs when another workflow's Execute Workflow node calls it. The last node's output is returned.
      </div>
    );
  }
  if (kind === "error") {
    return (
      <div className="info-box">
        Emits <code>workflow</code>, <code>execution</code>, <code>failedNode</code> and <code>error</code> of the failed run.
        Watching other workflows requires this workflow to be active.
      </div>
    );
  }
  if (kind === "manual") {
    return <div className="info-box">Select this node and click ▶ Run to start the workflow from here.</div>;
  }
  if (kind !== "webhook" && kind !== "form" && kind !== "schedule" && kind !== "poll") return null;
  if (!info) {
    return <div className="info-box">{dirty ? "Save the workflow to activate this trigger." : "Loading…"}</div>;
  }

  const status = info.listening ? (
    <span className="chip on">listening</span>
  ) : (
    <span className="chip">inactive – activate the workflow</span>
  );

  if (info.error) {
    return (
      <div className="info-box">
        {status} <span className="error-text">{info.error}</span>
      </div>
    );
  }

  if (info.webhook) {
    const w = info.webhook;
    const method = w.method === "ANY" ? "POST" : w.method;
    const auth =
      w.auth === "token"
        ? ` \\\n  -H '${w.header}: <secret>'`
        : w.auth === "hmac"
          ? ` \\\n  -H "${w.header}: sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac '<secret>' -r | cut -d' ' -f1)"`
          : "";
    const body = method === "GET" || method === "DELETE" ? "" : ` \\\n  -H 'Content-Type: application/json' -d "$BODY"`;
    const curl = `${body ? `BODY='{"hello":"world"}'\n` : ""}curl -X ${method} '${w.url}'${auth}${body}`;
    return (
      <div className="info-box">
        <div className="row">
          {status}
          <span className="chip">{w.method}</span>
          <span className="chip">{w.respond === "lastNode" ? "returns result" : "202 async"}</span>
        </div>
        <div className="field-label" style={{ marginTop: 6 }}>
          Webhook URL{dirty && <span className="muted"> (saved version)</span>}
        </div>
        <div className="row">
          <code className="url">{w.url}</code>
          <button className="btn small" onClick={() => navigator.clipboard?.writeText(w.url)}>
            Copy
          </button>
        </div>
        <details>
          <summary className="small">curl example</summary>
          <pre className="json">{curl}</pre>
        </details>
      </div>
    );
  }

  if (info.form) {
    const f = info.form;
    const protection = {
      none: "open to anyone",
      captcha: "captcha",
      login: "Keycloak login",
      email: "e-mail access code",
    }[f.protection];
    const who = {
      none: "Anyone with the link can answer",
      captcha: "Anyone with the link who solves a small sum can answer",
      login: "Members of your organization can answer after signing in with Keycloak",
      email: "Anyone who confirms an e-mail address with a code sent to it can answer",
    }[f.protection];
    return (
      <div className="info-box">
        <div className="row">
          {status}
          <span className="chip">
            {f.questions} question{f.questions === 1 ? "" : "s"}
          </span>
          <span className="chip">{protection}</span>
        </div>
        {f.warning && <div className="error-text" style={{ marginTop: 6 }}>{f.warning}</div>}
        <div className="field-label" style={{ marginTop: 6 }}>
          Form URL{dirty && <span className="muted"> (saved version)</span>}
        </div>
        <div className="row">
          <code className="url">{f.url}</code>
          <button className="btn small" onClick={() => navigator.clipboard?.writeText(f.url)}>
            Copy
          </button>
          <a className="btn small" href={f.url} target="_blank" rel="noreferrer">
            Open
          </a>
        </div>
        <p className="small" style={{ margin: "6px 0 0" }}>
          {who} while the workflow is active. Each submission starts a run with one item: the answers by question key,
          plus <code>submittedAt</code>
          {(f.protection === "login" || f.protection === "email") && (
            <>
              {" "}
              and <code>submittedBy</code>
            </>
          )}
          . Add <code>?key=value</code> to the link to prefill answers.
        </p>
      </div>
    );
  }

  if (info.poll) {
    const p = info.poll;
    return (
      <div className="info-box">
        <div className="row">
          {status}
          <span className="chip">every {p.intervalMinutes} min</span>
        </div>
        <p className="small" style={{ margin: "6px 0 0" }}>
          While the workflow is active, glue2flow checks for new items
          {info.listening && <> (next check {new Date(p.next).toLocaleTimeString()})</>} and starts a run only when there
          are some. The first check remembers what is already there. ▶ Run shows the current items without remembering
          them. Failed checks appear as failed executions.
        </p>
      </div>
    );
  }

  if (info.schedule) {
    const s = info.schedule;
    const fmt = new Intl.DateTimeFormat(undefined, {
      dateStyle: "medium",
      timeStyle: "short",
      timeZone: s.timezone,
    });
    return (
      <div className="info-box">
        <div className="row">
          {status}
          <span className="chip">{s.timezone}</span>
        </div>
        <div className="field-label" style={{ marginTop: 6 }}>
          {s.mode === "once" ? "Runs at" : "Next runs"}
          {dirty && <span className="muted"> (saved version)</span>}
        </div>
        {s.next.length === 0 ? (
          <div className="muted">{s.mode === "once" ? "This time is in the past; it will not run." : "No upcoming runs."}</div>
        ) : (
          <ul className="next-runs">
            {s.next.map((t) => (
              <li key={t}>{fmt.format(new Date(t))}</li>
            ))}
          </ul>
        )}
      </div>
    );
  }
  return null;
}
