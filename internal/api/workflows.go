package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/wolfposd/g8g/internal/auth"
	"github.com/wolfposd/g8g/internal/engine"
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
	if !wf.Access.AtLeast(workflow.RoleEditor) {
		redactSecrets(s.reg, wf)
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
		OrgID: u.Org, OwnerSub: u.Sub, OwnerName: u.Name,
	}
	if err := s.apply(wf, &in, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.CreateWorkflow(r.Context(), wf); err != nil {
		s.storeError(w, err)
		return
	}
	s.notifyChanged(wf)
	wf.Access = workflow.RoleOwner
	writeJSON(w, http.StatusCreated, wf)
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
	if err := s.apply(wf, &in, isOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateWorkflow(r.Context(), wf); err != nil {
		s.storeError(w, err)
		return
	}
	s.notifyChanged(wf)
	writeJSON(w, http.StatusOK, wf)
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
func (s *Server) apply(wf *workflow.Workflow, in *workflowInput, sharing bool) error {
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

// validateNodes lets node types check their own parameters (e.g. cron syntax).
func (s *Server) validateNodes(g *workflow.Graph) error {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		impl, _ := s.reg.Get(n.Type)
		if v, ok := impl.(nodes.Validator); ok && !n.Disabled {
			if err := v.Validate(n); err != nil {
				return errors.New(n.Name + ": " + err.Error())
			}
		}
	}
	return nil
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

// redactSecrets blanks secret parameters for users who cannot edit.
func redactSecrets(reg *nodes.Registry, wf *workflow.Workflow) {
	for i := range wf.Graph.Nodes {
		n := &wf.Graph.Nodes[i]
		impl, ok := reg.Get(n.Type)
		if !ok {
			continue
		}
		for _, p := range impl.Definition().Params {
			if p.Type == nodes.ParamSecret {
				if _, set := n.Params[p.Name]; set {
					n.Params[p.Name] = "••••••"
				}
			}
		}
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
