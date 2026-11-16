// Package credentials manages API-key credentials: named sets of field values
// (tokens, webhook URLs, account settings) that a user stores once for a
// module and selects in nodes, for services without OAuth2.
//
// Credentials are connections of kind "credential" (see store.Connection), so
// they share sharing rules, the node picker and the run-time organization
// check with OAuth2 accounts. Secret fields are sealed with an AAD bound to
// the connection and the field, never returned by the API, and only handed
// to the module while a node runs.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/glue2flow/g2f/internal/auth"
	"github.com/glue2flow/g2f/internal/nodes"
	"github.com/glue2flow/g2f/internal/secrets"
	"github.com/glue2flow/g2f/internal/store"
)

// ErrInvalid marks errors caused by the request (unknown module or field,
// missing required value, URL on a host the module does not allow).
var ErrInvalid = errors.New("invalid credential")

// ErrNotFound is returned when a node references a missing credential, or one
// of another organization or module.
var ErrNotFound = errors.New("credential not found")

// Store is the persistence the service needs.
type Store interface {
	CreateConnection(ctx context.Context, c *store.Connection) error
	GetConnection(ctx context.Context, id string) (*store.Connection, error)
	UpdateConnectionFields(ctx context.Context, c *store.Connection) error
}

type Service struct {
	store Store
	box   *secrets.Box
	reg   *nodes.Registry
}

func NewService(st Store, box *secrets.Box, reg *nodes.Registry) *Service {
	return &Service{store: st, box: box, reg: reg}
}

func (s *Service) spec(module string) (*nodes.ModuleInfo, *nodes.CredentialsSpec, error) {
	info, ok := s.reg.Module(module)
	if !ok || info.Credentials == nil {
		return nil, nil, fmt.Errorf("%w: plugin %q does not use credentials", ErrInvalid, module)
	}
	return info, info.Credentials, nil
}

// Create stores a new private credential of u for module. values maps field
// names to strings; see apply.
func (s *Service) Create(ctx context.Context, u *auth.User, module, name string, values map[string]any) (*store.Connection, error) {
	info, spec, err := s.spec(module)
	if err != nil {
		return nil, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = info.Name + " credential"
	}
	c := &store.Connection{
		ID: uuid.NewString(), OrgID: u.Org, Module: module, Kind: store.KindCredential, Name: name,
		OwnerSub: u.Sub, OwnerName: u.Name, Scope: "private", Groups: []string{},
	}
	if err := s.apply(spec, c, values); err != nil {
		return nil, err
	}
	if err := s.store.CreateConnection(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Update changes the fields of credential c; the caller checks that the user
// may manage it. Omitted fields and nodes.SecretRef keep their value.
func (s *Service) Update(ctx context.Context, c *store.Connection, values map[string]any) error {
	if c.Kind != store.KindCredential {
		return fmt.Errorf("%w: %q is not a credential", ErrInvalid, c.Name)
	}
	_, spec, err := s.spec(c.Module)
	if err != nil {
		return err
	}
	if err := s.apply(spec, c, values); err != nil {
		return err
	}
	return s.store.UpdateConnectionFields(ctx, c)
}

// apply merges values into c with the write-only semantics of node secrets:
// a string sets a field (secret fields are sealed), "" or null clears it, and
// an omitted field or nodes.SecretRef keeps the stored value. Unknown fields,
// non-string values and URLs on hosts the module does not allow are rejected,
// and required fields must end up set.
func (s *Service) apply(spec *nodes.CredentialsSpec, c *store.Connection, values map[string]any) error {
	fields, sealed := map[string]string{}, map[string]string{}
	for k, v := range c.Fields {
		fields[k] = v
	}
	for k, v := range c.SecretFields {
		sealed[k] = v
	}
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names) // stable error messages
	for _, name := range names {
		v := values[name]
		p := spec.Field(name)
		if p == nil {
			return fmt.Errorf("%w: unknown field %q", ErrInvalid, name)
		}
		if nodes.IsSecretRef(v) {
			continue
		}
		str, ok := v.(string)
		if v != nil && !ok {
			return fmt.Errorf("%w: field %q must be a string", ErrInvalid, name)
		}
		str = strings.TrimSpace(str)
		if str == "" {
			delete(fields, name)
			delete(sealed, name)
			continue
		}
		if err := spec.CheckValue(name, str); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if p.IsSecret() {
			box, err := s.box.Seal(str, secrets.CredentialAAD(c.ID, name))
			if err != nil {
				return err
			}
			sealed[name] = box
			delete(fields, name)
		} else {
			fields[name] = str
			delete(sealed, name)
		}
	}
	for _, p := range spec.Fields {
		_, set := fields[p.Name]
		if _, ok := sealed[p.Name]; ok {
			set = true
		}
		if p.Required && !set && p.Default == nil {
			return fmt.Errorf("%w: %s is required", ErrInvalid, p.Label)
		}
	}
	c.Fields, c.SecretFields = fields, sealed
	return nil
}

// SecretsSet reports which secret fields of c hold a value, for the API.
func SecretsSet(c *store.Connection) map[string]bool {
	if c.Kind != store.KindCredential {
		return nil
	}
	out := map[string]bool{}
	for k := range c.SecretFields {
		out[k] = true
	}
	return out
}

// Credential returns the decrypted field values of credential id for a node
// of module in org (defaults applied), and the secret values among them for
// scrubbing. Values of url fields are checked against the module's apiHosts
// again, since the manifest may have changed since the credential was saved.
func (s *Service) Credential(ctx context.Context, org, module, id string) (values map[string]any, secretValues []string, err error) {
	c, err := s.store.GetConnection(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (c.OrgID != org || c.Module != module || c.Kind != store.KindCredential)) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	_, spec, err := s.spec(module)
	if err != nil {
		return nil, nil, err
	}
	values = spec.Defaults()
	for _, p := range spec.Fields {
		var v string
		if sealed, ok := c.SecretFields[p.Name]; ok && p.IsSecret() {
			if v, err = s.box.Open(sealed, secrets.CredentialAAD(c.ID, p.Name)); err != nil {
				return nil, nil, fmt.Errorf("field %s of %q: %w", p.Name, c.Name, err)
			}
			secretValues = append(secretValues, v)
		} else if plain, ok := c.Fields[p.Name]; ok && !p.IsSecret() {
			v = plain
		} else if p.Required && p.Default == nil {
			return nil, nil, fmt.Errorf("credential %q has no %s; edit it on the Connections page", c.Name, p.Label)
		}
		if v == "" {
			continue
		}
		if err := spec.CheckValue(p.Name, v); err != nil {
			return nil, nil, fmt.Errorf("credential %q: %v", c.Name, err)
		}
		values[p.Name] = v
	}
	return values, secretValues, nil
}
