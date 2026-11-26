package core

import (
	"context"
	"fmt"
	"time"
)

// CreateGroup creates a logical group. Membership is not stored here: it lives
// on each Component via Spec.GroupID.
func (s *Service) CreateGroup(ctx context.Context, name string) (*Group, error) {
	g := &Group{
		ID:        newID(),
		Name:      name,
		CreatedAt: time.Now(),
	}
	if err := s.store.SaveGroup(g); err != nil {
		return nil, err
	}
	s.log.Info("created group", "id", g.ID, "name", name)
	return g, nil
}

// GetGroup returns a group by id.
func (s *Service) GetGroup(ctx context.Context, id string) (*Group, error) {
	return s.store.GetGroup(id)
}

// ListGroups returns all groups.
func (s *Service) ListGroups(ctx context.Context) ([]*Group, error) {
	return s.store.ListGroups()
}

// GroupMembers returns the components whose GroupID matches the group id.
func (s *Service) GroupMembers(ctx context.Context, groupID string) ([]*Component, error) {
	comps, err := s.store.ListComponents()
	if err != nil {
		return nil, err
	}
	out := make([]*Component, 0)
	for _, c := range comps {
		if c.Spec.GroupID == groupID {
			out = append(out, c)
		}
	}
	return out, nil
}

// DeleteGroup deletes a group. When cascade is true, its member components are
// removed (container + record) first; otherwise members simply lose their
// GroupID via nothing (the group record just disappears and members keep a
// dangling GroupID until reassigned). A single member failure does not abort.
func (s *Service) DeleteGroup(ctx context.Context, id string, cascade bool) ([]BatchResult, error) {
	if _, err := s.store.GetGroup(id); err != nil {
		return nil, err
	}
	var results []BatchResult
	if cascade {
		members, err := s.GroupMembers(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			res := BatchResult{ComponentID: m.ID, Name: m.Spec.Name}
			if err := s.Remove(ctx, m.ID); err != nil {
				res.Err = err.Error()
			} else {
				res.Phase = "Removed"
			}
			results = append(results, res)
		}
	}
	if err := s.store.DeleteGroup(id); err != nil {
		return results, err
	}
	s.log.Info("deleted group", "id", id, "cascade", cascade)
	return results, nil
}

// AddMember assigns a component to a group by setting its GroupID.
func (s *Service) AddMember(ctx context.Context, groupID, componentID string) (*Component, error) {
	if _, err := s.store.GetGroup(groupID); err != nil {
		return nil, fmt.Errorf("group %s: %w", groupID, err)
	}
	return s.setGroupID(componentID, groupID, "")
}

// RemoveMember clears a component's group membership. It fails if the
// component is not currently a member of the addressed group, so a request
// against the wrong group cannot silently detach the component.
func (s *Service) RemoveMember(ctx context.Context, groupID, componentID string) (*Component, error) {
	return s.setGroupID(componentID, "", groupID)
}

func (s *Service) setGroupID(componentID, groupID, expectCurrent string) (*Component, error) {
	lock := s.lockFor(componentID)
	lock.Lock()
	defer lock.Unlock()

	comp, err := s.store.GetComponent(componentID)
	if err != nil {
		return nil, err
	}
	if expectCurrent != "" && comp.Spec.GroupID != expectCurrent {
		return nil, fmt.Errorf("component %s is not a member of group %s: %w", componentID, expectCurrent, ErrNotFound)
	}
	comp.Spec.GroupID = groupID
	comp.UpdatedAt = time.Now()
	if err := s.store.SaveComponent(comp); err != nil {
		return nil, err
	}
	return comp, nil
}

// StartGroup starts every member of a group. Each member is attempted
// independently; one failure is recorded but does not stop the rest.
func (s *Service) StartGroup(ctx context.Context, id string) ([]BatchResult, error) {
	return s.batch(ctx, id, s.Start)
}

// StopGroup stops every member of a group (same non-aborting semantics).
func (s *Service) StopGroup(ctx context.Context, id string) ([]BatchResult, error) {
	return s.batch(ctx, id, s.Stop)
}

func (s *Service) batch(ctx context.Context, id string, op func(context.Context, string) (*Component, error)) ([]BatchResult, error) {
	if _, err := s.store.GetGroup(id); err != nil {
		return nil, err
	}
	members, err := s.GroupMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	results := make([]BatchResult, 0, len(members))
	for _, m := range members {
		res := BatchResult{ComponentID: m.ID, Name: m.Spec.Name}
		comp, err := op(ctx, m.ID)
		if err != nil {
			res.Err = err.Error()
			res.Phase = m.Status.Phase
		} else {
			res.Phase = comp.Status.Phase
		}
		results = append(results, res)
	}
	return results, nil
}
