import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  addEdge,
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useEdgesState,
  useNodesState,
  useReactFlow,
  type Connection,
  type Edge,
  type EdgeChange,
  type NodeChange,
} from "@xyflow/react";
import { api, ApiError } from "../api";
import { navigate } from "../App";
import { FlowNode, type FlowNodeData, type FlowNodeType } from "../components/FlowNode";
import { defaultParams, ParamPanel } from "../components/ParamPanel";
import { SharingDialog } from "../components/SharingDialog";
import { ExecutionsPanel } from "../components/ExecutionsPanel";
import { ResizableSide } from "../components/ResizableSide";
import { TriggerInfo } from "../components/TriggerInfo";
import { NodePalette } from "../components/NodePalette";
import { atLeast, type ClientConfig, type Execution, type Graph, type NodeDef, type User, type Workflow } from "../types";

const nodeTypes = { g2f: FlowNode };

interface Props {
  id: string;
  defs: NodeDef[];
  user: User;
  config: ClientConfig;
}

export function Editor(props: Props) {
  return (
    <ReactFlowProvider>
      <EditorInner {...props} />
    </ReactFlowProvider>
  );
}

function toFlow(graph: Graph, defs: Map<string, NodeDef>): { nodes: FlowNodeType[]; edges: Edge[] } {
  return {
    nodes: graph.nodes.map((n) => ({
      id: n.id,
      type: "g2f",
      position: n.position,
      data: { type: n.type, name: n.name, params: n.params ?? {}, disabled: n.disabled, def: defs.get(n.type) },
    })),
    edges: graph.edges.map((e) => ({
      ...e,
      sourceHandle: e.sourceHandle ?? undefined,
      targetHandle: e.targetHandle ?? undefined,
    })),
  };
}

function fromFlow(nodes: FlowNodeType[], edges: Edge[]): Graph {
  return {
    nodes: nodes.map((n) => ({
      id: n.id,
      type: n.data.type,
      name: n.data.name,
      position: { x: Math.round(n.position.x), y: Math.round(n.position.y) },
      params: n.data.params,
      disabled: n.data.disabled || undefined,
    })),
    edges: edges.map((e) => ({
      id: e.id,
      source: e.source,
      sourceHandle: e.sourceHandle ?? undefined,
      target: e.target,
      targetHandle: e.targetHandle ?? undefined,
    })),
  };
}

function EditorInner({ id, defs, user, config }: Props) {
  const defMap = useMemo(() => new Map(defs.map((d) => [d.type, d])), [defs]);
  const [wf, setWf] = useState<Workflow>();
  const [nodes, setNodes, onNodesChangeRaw] = useNodesState<FlowNodeType>([]);
  const [edges, setEdges, onEdgesChangeRaw] = useEdgesState<Edge>([]);
  const [dirty, setDirty] = useState(false);
  const [selectedId, setSelectedId] = useState<string>();
  const [executions, setExecutions] = useState<Execution[]>([]);
  const [execution, setExecution] = useState<Execution>();
  const [showSharing, setShowSharing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ kind: "error" | "info"; text: string }>();
  const { screenToFlowPosition } = useReactFlow();
  const pollRef = useRef<number>(undefined);

  const canEdit = atLeast(wf?.access, "editor");
  const canRun = atLeast(wf?.access, "runner");
  const isOwner = wf?.access === "owner";

  const load = useCallback(async () => {
    try {
      const w = await api.workflow(id);
      setWf(w);
      const f = toFlow(w.graph, defMap);
      setNodes(f.nodes);
      setEdges(f.edges);
      setDirty(false);
    } catch (e) {
      setMessage({ kind: "error", text: (e as Error).message });
    }
  }, [id, defMap, setNodes, setEdges]);

  const loadExecutions = useCallback(() => {
    api
      .executions(id)
      .then(setExecutions)
      .catch(() => {});
  }, [id]);

  useEffect(() => {
    load();
    loadExecutions();
    return () => window.clearTimeout(pollRef.current);
  }, [load, loadExecutions]);

  // Warn before leaving with unsaved changes.
  useEffect(() => {
    const h = (e: BeforeUnloadEvent) => {
      if (dirty) e.preventDefault();
    };
    window.addEventListener("beforeunload", h);
    return () => window.removeEventListener("beforeunload", h);
  }, [dirty]);

  // Paint execution status onto nodes.
  useEffect(() => {
    const byNode = new Map(execution?.steps?.map((s) => [s.nodeId, s]));
    setNodes((ns) =>
      ns.map((n) => {
        const s = byNode.get(n.id);
        const count = s?.output ? Object.values(s.output).reduce((a, l) => a + l.length, 0) : undefined;
        if (n.data.status === s?.status && n.data.itemCount === count) return n;
        return { ...n, data: { ...n.data, status: s?.status, itemCount: count } };
      }),
    );
  }, [execution, setNodes]);

  const onNodesChange = useCallback(
    (changes: NodeChange<FlowNodeType>[]) => {
      const editable = canEdit ? changes : changes.filter((c) => c.type === "select" || c.type === "dimensions");
      if (editable.some((c) => c.type !== "select" && c.type !== "dimensions")) setDirty(true);
      for (const c of editable) if (c.type === "select" && c.selected) setSelectedId(c.id);
      onNodesChangeRaw(editable);
    },
    [canEdit, onNodesChangeRaw],
  );

  const onEdgesChange = useCallback(
    (changes: EdgeChange[]) => {
      if (!canEdit) return onEdgesChangeRaw(changes.filter((c) => c.type === "select"));
      if (changes.some((c) => c.type !== "select")) setDirty(true);
      onEdgesChangeRaw(changes);
    },
    [canEdit, onEdgesChangeRaw],
  );

  const onConnect = useCallback(
    (c: Connection) => {
      if (!canEdit) return;
      setEdges((es) => addEdge({ ...c, id: `e-${crypto.randomUUID().slice(0, 8)}` }, es));
      setDirty(true);
    },
    [canEdit, setEdges],
  );

  const uniqueName = (base: string) => {
    const names = new Set(nodes.map((n) => n.data.name));
    if (!names.has(base)) return base;
    for (let i = 2; ; i++) if (!names.has(`${base} ${i}`)) return `${base} ${i}`;
  };

  const addNode = (def: NodeDef, position?: { x: number; y: number }) => {
    const nid = `${def.type.split(".").pop()}-${crypto.randomUUID().slice(0, 6)}`;
    const pos = position ?? screenToFlowPosition({ x: window.innerWidth / 2 - 100, y: window.innerHeight / 3 });
    setNodes((ns) => [
      ...ns.map((n) => ({ ...n, selected: false })),
      {
        id: nid,
        type: "g2f",
        position: pos,
        selected: true,
        data: { type: def.type, name: uniqueName(def.name), params: defaultParams(def), def },
      },
    ]);
    setSelectedId(nid);
    setDirty(true);
  };

  const updateNode = (nid: string, patch: Partial<FlowNodeData>) => {
    setNodes((ns) => ns.map((n) => (n.id === nid ? { ...n, data: { ...n.data, ...patch } } : n)));
    setDirty(true);
  };

  const deleteNode = (nid: string) => {
    setNodes((ns) => ns.filter((n) => n.id !== nid));
    setEdges((es) => es.filter((e) => e.source !== nid && e.target !== nid));
    setSelectedId(undefined);
    setDirty(true);
  };

  async function save(patch: Partial<Workflow> = {}): Promise<Workflow | undefined> {
    if (!wf) return;
    setBusy(true);
    setMessage(undefined);
    try {
      const next = { ...wf, ...patch };
      const saved = await api.updateWorkflow(wf.id, {
        name: next.name,
        description: next.description,
        scope: next.scope,
        orgRole: next.orgRole,
        shares: next.shares,
        active: next.active,
        version: wf.version,
        graph: fromFlow(nodes, edges),
      });
      setWf({ ...saved, access: wf.access });
      setDirty(false);
      return saved;
    } catch (e) {
      const text = e instanceof ApiError && e.status === 409 ? `${e.message}` : (e as Error).message;
      setMessage({ kind: "error", text });
    } finally {
      setBusy(false);
    }
  }

  const poll = useCallback(
    (execId: string) => {
      window.clearTimeout(pollRef.current);
      api
        .execution(execId)
        .then((ex) => {
          setExecution(ex);
          if (ex.status === "queued" || ex.status === "running") {
            pollRef.current = window.setTimeout(() => poll(execId), 700);
          } else {
            loadExecutions();
          }
        })
        .catch((e) => setMessage({ kind: "error", text: e.message }));
    },
    [loadExecutions],
  );

  async function run() {
    if (!wf) return;
    if (dirty && canEdit && !(await save())) return;
    const sel = nodes.find((n) => n.id === selectedId);
    const triggerId = sel?.data.def?.trigger ? sel.id : undefined;
    try {
      const ex = await api.run(wf.id, triggerId);
      setExecution(ex);
      poll(ex.id);
    } catch (e) {
      setMessage({ kind: "error", text: (e as Error).message });
    }
  }

  async function remove() {
    if (!wf || !confirm(`Delete workflow "${wf.name}"? This also deletes its execution history.`)) return;
    try {
      await api.deleteWorkflow(wf.id);
      navigate("/");
    } catch (e) {
      setMessage({ kind: "error", text: (e as Error).message });
    }
  }

  if (!wf) return message ? <div className="alert error">{message.text}</div> : <div className="loading">Loading…</div>;

  const selected = nodes.find((n) => n.id === selectedId);
  const hasTrigger = nodes.some((n) => n.data.def?.trigger);

  return (
    <div className="editor">
      <div className="toolbar">
        <a className="btn ghost" href="#/" onClick={(e) => dirty && !confirm("Discard unsaved changes?") && e.preventDefault()}>
          ←
        </a>
        <input
          className="title-input big"
          value={wf.name}
          disabled={!canEdit}
          onChange={(e) => {
            setWf({ ...wf, name: e.target.value });
            setDirty(true);
          }}
        />
        <button className={`chip clickable scope-${wf.scope}`} onClick={() => setShowSharing(true)} title="Sharing">
          {wf.scope === "private" ? "🔒 Private" : wf.scope === "group" ? "👥 Groups" : "🏢 Organization"}
        </button>
        <span className="muted small">
          {wf.access} · v{wf.version}
        </span>
        <div className="spacer" />
        {message && <span className={message.kind === "error" ? "error-text small" : "muted small"}>{message.text}</span>}
        {canEdit && (
          <div className="active-toggle" title="Active workflows react to schedules and webhooks">
            <span className="muted small">Active</span>
            <div className="pill-toggle" role="group">
              <button
                type="button"
                className={wf.active ? "yes sel" : "yes"}
                disabled={busy}
                onClick={() => !wf.active && save({ active: true })}
              >
                yes
              </button>
              <button
                type="button"
                className={!wf.active ? "no sel" : "no"}
                disabled={busy}
                onClick={() => wf.active && save({ active: false })}
              >
                no
              </button>
            </div>
          </div>
        )}
        {canEdit && (
          <button className="btn" disabled={!dirty || busy} onClick={() => save()}>
            {dirty ? "Save" : "Saved"}
          </button>
        )}
        {canRun && (
          <button
            className="btn primary"
            disabled={busy || !hasTrigger}
            onClick={run}
            title="Run from the selected trigger (or the first one)"
          >
            ▶ Run
          </button>
        )}
        {isOwner && (
          <button className="btn ghost danger" onClick={remove} title="Delete workflow">
            🗑
          </button>
        )}
      </div>

      <div className="editor-body">
        {canEdit && <NodePalette defs={defs} onAdd={(d) => addNode(d)} />}

        <div
          className="canvas"
          onDragOver={(e) => {
            e.preventDefault();
            e.dataTransfer.dropEffect = "move";
          }}
          onDrop={(e) => {
            e.preventDefault();
            const def = defMap.get(e.dataTransfer.getData("application/g2f-node"));
            if (def && canEdit) addNode(def, screenToFlowPosition({ x: e.clientX, y: e.clientY }));
          }}
        >
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onPaneClick={() => setSelectedId(undefined)}
            nodesConnectable={canEdit}
            nodesDraggable={canEdit}
            deleteKeyCode={canEdit ? ["Backspace", "Delete"] : null}
            defaultEdgeOptions={{ animated: false }}
            fitView
            fitViewOptions={{ maxZoom: 1.2 }}
            proOptions={{ hideAttribution: true }}
          >
            <Background gap={16} />
            <Controls />
            <MiniMap pannable zoomable />
          </ReactFlow>
        </div>

        {selected && (
          <ResizableSide>
            <ParamPanel
              key={selected.id}
              nodeId={selected.id}
              data={selected.data}
              readOnly={!canEdit}
              extra={<TriggerInfo workflow={wf} nodeId={selected.id} data={selected.data} config={config} dirty={dirty} />}
              onChange={(p) => updateNode(selected.id, p)}
              onDelete={() => deleteNode(selected.id)}
            />
          </ResizableSide>
        )}
      </div>

      <ExecutionsPanel
        executions={executions}
        selected={execution}
        canCancel={canRun}
        onSelect={(eid) => poll(eid)}
        onRefresh={loadExecutions}
        onCancel={async (eid) => {
          try {
            await api.cancelExecution(eid);
            poll(eid);
            loadExecutions();
          } catch (e) {
            setMessage({ kind: "error", text: (e as Error).message });
          }
        }}
      />

      {showSharing && (
        <SharingDialog
          user={user}
          scope={wf.scope}
          orgRole={wf.orgRole}
          shares={wf.shares}
          canEdit={isOwner}
          onClose={() => setShowSharing(false)}
          onSave={async (v) => {
            setShowSharing(false);
            await save(v);
          }}
        />
      )}
    </div>
  );
}
