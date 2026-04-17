package supervisor

import (
	"slices"
	"strings"
)

// aclSelector is a parsed selector from an ACL list entry. Exactly one of Any,
// ID, or Tags is populated.
type aclSelector struct {
	Any  bool
	ID   string
	Tags []string
}

func parseSelectors(src []string) []aclSelector {
	out := make([]aclSelector, 0, len(src))
	for _, raw := range src {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if raw == "*" {
			out = append(out, aclSelector{Any: true})
			continue
		}
		if strings.HasPrefix(raw, "id:") {
			id := strings.TrimSpace(strings.TrimPrefix(raw, "id:"))
			if id != "" {
				out = append(out, aclSelector{ID: id})
			}
			continue
		}
		if strings.HasPrefix(raw, "tag:") {
			rest := strings.TrimPrefix(raw, "tag:")
			var tags []string
			for t := range strings.SplitSeq(rest, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					tags = append(tags, t)
				}
			}
			if len(tags) > 0 {
				out = append(out, aclSelector{Tags: tags})
			}
			continue
		}
		// Unqualified treated as an id for convenience: `sends_to: [implementer]`.
		out = append(out, aclSelector{ID: raw})
	}
	return out
}

func (s aclSelector) matches(id string, tags []string) bool {
	if s.Any {
		return true
	}
	if s.ID != "" {
		return s.ID == id
	}
	if len(s.Tags) > 0 {
		for _, t := range s.Tags {
			if !slices.Contains(tags, t) {
				return false
			}
		}
		return true
	}
	return false
}

// aclAllows returns whether any selector in the list matches (id,tags). A nil
// or empty list is permissive — the caller doesn't care about ACL enforcement.
func aclAllows(list []string, id string, tags []string) bool {
	if len(list) == 0 {
		return true
	}
	for _, sel := range parseSelectors(list) {
		if sel.matches(id, tags) {
			return true
		}
	}
	return false
}

// canSendLocked reports whether sender may address the target per sender's
// ACL. Caller must hold s.mu (R or W). Unknown/empty sender bypasses sender-
// side enforcement — recipient-side still applies via canReceiveLocked.
func (s *Supervisor) canSendLocked(senderID, targetID string, targetTags []string) bool {
	if senderID == "" {
		return true
	}
	sender, ok := s.agents[senderID]
	if !ok {
		return true
	}
	if sender.info.Spec.ACL == nil {
		return true
	}
	return aclAllows(sender.info.Spec.ACL.SendsTo, targetID, targetTags)
}

// canReceiveLocked reports whether target accepts from sender per target's
// accepts_from ACL. Caller must hold s.mu.
func (s *Supervisor) canReceiveLocked(senderID string, senderTags []string, targetID string) bool {
	target, ok := s.agents[targetID]
	if !ok {
		return true
	}
	if target.info.Spec.ACL == nil {
		return true
	}
	return aclAllows(target.info.Spec.ACL.AcceptsFrom, senderID, senderTags)
}

// ReachableFrom returns the set of agent ids senderID may address, filtered by
// both its sends_to and each candidate's accepts_from. Used by registry
// endpoints to narrow list_agents for the caller.
func (s *Supervisor) ReachableFrom(senderID string) map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var senderTags []string
	if senderID != "" {
		if rec, ok := s.agents[senderID]; ok {
			senderTags = rec.info.Spec.Tags
		}
	}
	out := map[string]struct{}{}
	for id, rec := range s.agents {
		if id == senderID {
			continue
		}
		if !s.canSendLocked(senderID, id, rec.info.Spec.Tags) {
			continue
		}
		if !s.canReceiveLocked(senderID, senderTags, id) {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

// senderTagsLocked returns the spec tags for senderID. Caller holds s.mu.
func (s *Supervisor) senderTagsLocked(senderID string) []string {
	if senderID == "" {
		return nil
	}
	if rec, ok := s.agents[senderID]; ok {
		return rec.info.Spec.Tags
	}
	return nil
}
