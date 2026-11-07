package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/engine"
	"github.com/wolfposd/g8g/internal/expression"
	"github.com/wolfposd/g8g/internal/nodes"
	"github.com/wolfposd/g8g/internal/store"
	"github.com/wolfposd/g8g/internal/workflow"
)

type workflowInput struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Scope       workflow.Scope   `json:"scope"`
	OrgRole     workflow.Role    `json:"orgRole"`
	Shares      []workflow.Share `json:"shares"`
	Graph       workflow.Graph   `json:"graph"`
	Active      bool             `json:"active"`
	Version     int              `json:"version"`
}

// workflowSummary is the list representation (no graph).
type workflowSummary struct {
	*workflow.Workflow
	Graph    *struct{} `json:"graph,omitempty"`
	Triggers []string  `json:"triggers"`
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	wfs, err := s.store.ListVisibleWorkflows(r.Context(), u)
	if err != nil {
		s.storeError(w, err)
		return
	}
	out := make([]workflowSummary, 0, len(wfs))
	for _, wf := range wfs {
		if wf.Access = wf.RoleFor(u); wf.Access == workflow.RoleNone {
			continue
		}
		var triggers []string
		for _, n := range wf.Graph.Nodes {
			if k := s.reg.TriggerKind(n.Type); k != "" && !n.Disabled && !slices.Contains(triggers, string(k)) {
				triggers = append(triggers, string(k))
			}
		}
		out = append(out, workflowSummary{Workflow: wf, Triggers: triggers})
	}
	writeJSON(w, http.StatusOK, out)
}

// loadWorkflow fetches a workflow and checks the caller has at least min.
func (s *Server) loadWorkflow(w http.ResponseWriter, r *http.Request, id string, min workflow.Role) (*workflow.Workflow, bool) {
	u := auth.FromContext(r.Context())
	wf, err := s.store.GetWorkflow(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return nil, false
	}
	wf.Access = wf.RoleFor(u)
	if wf.Access == workflow.RoleNone {
		// Do not reveal existence.
		writeError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	if !wf.Access.AtLeast(min) {
		writeError(w, http.StatusForbidden, "requires role "+string(min))
		return nil, false
	}
	return wf, true
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.loadWorkflow(w, r, chi.URLParam(r, "id"), workflow.RoleViewer)
	if !ok {
		return
	}
	if err := s.secrets.MarkStored(r.Context(), wf); err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	var in workflowInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	wf := &workflow.Workflow{
		ID: uuid.NewString(), OrgID: u.Org, OwnerSub: u.Sub, OwnerName: u.Name,
	}
	if err := s.apply(r.Context(), u, wf, &in, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ops, err := s.secrets.ExtractNodeSecrets(wf)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.CreateWorkflow(r.Context(), wf, ops); err != nil {
		s.storeError(w, err)
		return
	}
	s.notifyChanged(wf)
	wf.Access = workflow.RoleOwner
	s.respondWorkflow(w, r, http.StatusCreated, wf)
}

func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.loadWorkflow(w, r, chi.URLParam(r, "id"), workflow.RoleEditor)
	if !ok {
		return
	}
	var in workflowInput
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.Version != wf.Version {
		s.storeError(w, store.ErrConflict)
		return
	}
	isOwner := wf.Access == workflow.RoleOwner
	if !isOwner && sharingChanged(wf, &in) {
		writeError(w, http.StatusForbidden, "only the owner can change scope and sharing")
		return
	}
	if err := s.apply(r.Context(), auth.FromContext(r.Context()), wf, &in, isOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ops, err := s.secrets.ExtractNodeSecrets(wf)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateWorkflow(r.Context(), wf, ops); err != nil {
		s.storeError(w, err)
		return
	}
	s.notifyChanged(wf)
	s.respondWorkflow(w, r, http.StatusOK, wf)
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.loadWorkflow(w, r, id, workflow.RoleOwner); !ok {
		return
	}
	if err := s.store.DeleteWorkflow(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	if s.listener != nil {
		s.listener.WorkflowDeleted(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// apply copies validated input onto wf.
func (s *Server) apply(ctx context.Context, u *auth.User, wf *workflow.Workflow, in *workflowInput, sharing bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return errors.New("name is required")
	}
	if err := in.Graph.Validate(s.reg.Exists, s.reg.IsTrigger); err != nil {
		return err
	}
	if err := s.validateNodes(&in.Graph); err != nil {
		return err
	}
	if err := s.checkWorkflowRefs(ctx, u, wf.ID, &in.Graph); err != nil {
		return err
	}
	if err := s.checkConnectionRefs(ctx, u, &wf.Graph, &in.Graph); err != nil {
		return err
	}
	wf.Name, wf.Description, wf.Graph, wf.Active = in.Name, in.Description, in.Graph, in.Active
	if sharing {
		wf.Scope, wf.OrgRole, wf.Shares = in.Scope, in.OrgRole, in.Shares
		if wf.Shares == nil {
			wf.Shares = []workflow.Share{}
		}
		if err := wf.ValidateSharing(); err != nil {
			return err
		}
	}
	return nil
}

// respondWorkflow writes wf with stored secrets marked (never their values).
func (s *Server) respondWorkflow(w http.ResponseWriter, r *http.Request, status int, wf *workflow.Workflow) {
	if err := s.secrets.MarkStored(r.Context(), wf); err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, status, wf)
}

// validateNodes checks edge handles against node definitions and lets node
// types check their own parameters (e.g. cron syntax).
func (s *Server) validateNodes(g *workflow.Graph) error {
	for i := range g.Edges {
		e := &g.Edges[i]
		src, _ := s.reg.Get(g.Node(e.Source).Type)
		dst, _ := s.reg.Get(g.Node(e.Target).Type)
		var err error
		srcDef := src.Definition()
		if e.SourceHandle, err = pickHandle(e.SourceHandle, srcDef.OutputsFor(g.Node(e.Source).Params), "output", g.Node(e.Source).Name); err != nil {
			return err
		}
		if e.TargetHandle, err = pickHandle(e.TargetHandle, dst.Definition().Inputs, "input", g.Node(e.Target).Name); err != nil {
			return err
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		impl, _ := s.reg.Get(n.Type)
		if err := checkFieldExpressions(n, impl.Definition()); err != nil {
			return err
		}
		if v, ok := impl.(nodes.Validator); ok && !n.Disabled {
			if err := v.Validate(n); err != nil {
				return errors.New(n.Name + ": " + err.Error())
			}
		}
	}
	return nil
}

// checkFieldExpressions rejects node fields that try to read plugin config.
// Config (tokens, client secrets, base URLs) is reserved for the plugin;
// editors use vars for organization-wide values.
func checkFieldExpressions(n *workflow.Node, def nodes.Definition) error {
	for name, v := range n.Params {
		if p := def.Param(name); p != nil && (p.NoExpression || p.Type == nodes.ParamSecret) {
			continue
		}
		ids, err := expression.Identifiers(v)
		if err != nil {
			return fmt.Errorf("node %q, field %q: %w", n.Name, name, err)
		}
		for _, id := range ids {
			if id == "config" {
				return fmt.Errorf("node %q, field %q: config.* is only available to the plugin itself; use vars.* for values meant for node fields", n.Name, name)
			}
		}
	}
	return nil
}

// checkWorkflowRefs makes sure the saving user may use the workflows a node
// references: Execute Workflow needs the runner role on its target, an Error
// Trigger watching other workflows needs at least viewer. This keeps users
// from running or observing workflows that were not shared with them.
func (s *Server) checkWorkflowRefs(ctx context.Context, u *auth.User, selfID string, g *workflow.Graph) error {
	for _, n := range g.Nodes {
		impl, _ := s.reg.Get(n.Type)
		def := impl.Definition()
		for _, p := range def.Params {
			var ids []string
			var min workflow.Role
			switch p.Type {
			case nodes.ParamWorkflow:
				if id, _ := n.Params[p.Name].(string); id != "" {
					ids, min = []string{id}, workflow.RoleRunner
				}
			case nodes.ParamWorkflows:
				ids, min = nodes.ParseList(n.Params[p.Name]), workflow.RoleViewer
			default:
				continue
			}
			for _, id := range ids {
				if id == selfID {
					continue
				}
				ref, err := s.store.GetWorkflow(ctx, id)
				if err != nil || !ref.RoleFor(u).AtLeast(min) {
					return fmt.Errorf("node %q: workflow %s not found or not shared with you as %s", n.Name, id, min)
				}
			}
		}
	}
	return nil
}

// checkConnectionRefs makes sure every newly selected account is usable by the
// saving user and belongs to the node's plugin. Unchanged selections are not
// re-checked, so co-editors can save a workflow that uses someone else's
// shared account without having access to it themselves.
func (s *Server) checkConnectionRefs(ctx context.Context, u *auth.User, old, g *workflow.Graph) error {
	for _, n := range g.Nodes {
		id, _ := n.Params[nodes.ConnectionParam].(string)
		impl, _ := s.reg.Get(n.Type)
		def := impl.Definition()
		if id == "" || def.Param(nodes.ConnectionParam) == nil {
			continue
		}
		if prev := old.Node(n.ID); prev != nil && prev.Type == n.Type {
			if prevID, _ := prev.Params[nodes.ConnectionParam].(string); prevID == id {
				continue
			}
		}
		c, err := s.store.GetConnection(ctx, id)
		if err != nil || !c.UsableBy(u) || c.Module != def.Module {
			return fmt.Errorf("node %q: account not found or not shared with you", n.Name)
		}
	}
	return nil
}

// pickHandle defaults an empty handle to the node's only handle (or to "main"),
// and rejects handles the node does not have.
func pickHandle(h string, handles []string, kind, node string) (string, error) {
	if h == "" {
		if len(handles) == 1 {
			return handles[0], nil
		}
		if slices.Contains(handles, workflow.DefaultHandle) {
			return workflow.DefaultHandle, nil // e.g. Wait: main vs. timeout
		}
		return "", fmt.Errorf("connection to %q must name the %s (%s)", node, kind, strings.Join(handles, ", "))
	}
	if !slices.Contains(handles, h) {
		return "", fmt.Errorf("node %q has no %s %q", node, kind, h)
	}
	return h, nil
}

func sharingChanged(wf *workflow.Workflow, in *workflowInput) bool {
	if in.Scope != wf.Scope || (in.OrgRole != "" && in.OrgRole != wf.OrgRole) || len(in.Shares) != len(wf.Shares) {
		return true
	}
	for i := range in.Shares {
		if auth.NormalizeGroup(in.Shares[i].Group) != wf.Shares[i].Group || in.Shares[i].Role != wf.Shares[i].Role {
			return true
		}
	}
	return false
}

func (s *Server) notifyChanged(wf *workflow.Workflow) {
	if s.listener != nil {
		s.listener.WorkflowChanged(wf)
	}
}

type runInput struct {
	NodeID string       `json:"nodeId"`
	Items  []nodes.Item `json:"items"`
}

func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) {
	wf, ok := s.loadWorkflow(w, r, chi.URLParam(r, "id"), workflow.RoleRunner)
	if !ok {
		return
	}
	var in runInput
	if r.ContentLength != 0 {
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	u := auth.FromContext(r.Context())
	exec, err := s.engine.Start(r.Context(), engine.Request{
		Workflow: wf, TriggerType: nodes.TriggerManual, TriggerNode: in.NodeID,
		Items: in.Items, TriggeredBy: u.Name,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, exec)
}

func (s *Server) listExecutions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := s.loadWorkflow(w, r, id, workflow.RoleViewer); !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	execs, err := s.store.ListExecutions(r.Context(), id, limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, execs)
}

// cancelExecution stops a waiting execution (e.g. an approval nobody will answer).
func (s *Server) cancelExecution(w http.ResponseWriter, r *http.Request) {
	exec, err := s.store.GetExecution(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if _, ok := s.loadWorkflow(w, r, exec.WorkflowID, workflow.RoleRunner); !ok {
		return
	}
	if err := s.store.CancelWaiting(r.Context(), exec.ID, auth.FromContext(r.Context()).Name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "execution is not waiting")
			return
		}
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getExecution(w http.ResponseWriter, r *http.Request) {
	exec, err := s.store.GetExecution(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if _, ok := s.loadWorkflow(w, r, exec.WorkflowID, workflow.RoleViewer); !ok {
		return
	}
	writeJSON(w, http.StatusOK, exec)
}
