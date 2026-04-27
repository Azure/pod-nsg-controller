package engine

import (
	"sort"
)

// ComputeDiff computes the list of create/update/delete actions needed
// to reconcile desired state with actual state.
func ComputeDiff(
	desired map[ASGTarget]DesiredPrefixSet,
	actual map[ASGTarget]ActualPrefixSet,
) []Action {
	if desired == nil {
		desired = make(map[ASGTarget]DesiredPrefixSet)
	}
	if actual == nil {
		actual = make(map[ASGTarget]ActualPrefixSet)
	}

	// Build normalized lookup maps to handle case-insensitive Azure resource IDs.
	type desiredEntry struct {
		target ASGTarget
		ips    map[string]struct{}
	}
	type actualEntry struct {
		target ASGTarget
		ips    map[string]struct{}
	}

	desiredByKey := make(map[string]desiredEntry, len(desired))
	for t, dps := range desired {
		desiredByKey[targetKey(t)] = desiredEntry{target: t, ips: dps.IPs}
	}

	actualByKey := make(map[string]actualEntry, len(actual))
	for t, aps := range actual {
		actualByKey[targetKey(t)] = actualEntry{target: t, ips: aps.IPs}
	}

	actions := make([]Action, 0)

	for key, de := range desiredByKey {
		if ae, exists := actualByKey[key]; exists {
			if !ipSetsEqual(de.ips, ae.ips) {
				actions = append(actions, Action{
					Kind:       UpdatePrefixSet,
					Target:     de.target,
					DesiredIPs: sortedIPs(de.ips),
				})
			}
		} else {
			actions = append(actions, Action{
				Kind:       CreatePrefixSet,
				Target:     de.target,
				DesiredIPs: sortedIPs(de.ips),
			})
		}
	}

	for key, ae := range actualByKey {
		if _, exists := desiredByKey[key]; !exists {
			actions = append(actions, Action{
				Kind:   DeletePrefixSet,
				Target: ae.target,
			})
		}
	}

	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		if a.Target.SubscriptionID != b.Target.SubscriptionID {
			return a.Target.SubscriptionID < b.Target.SubscriptionID
		}
		if a.Target.ResourceGroup != b.Target.ResourceGroup {
			return a.Target.ResourceGroup < b.Target.ResourceGroup
		}
		if a.Target.ASGName != b.Target.ASGName {
			return a.Target.ASGName < b.Target.ASGName
		}
		if a.Target.PrefixSetName != b.Target.PrefixSetName {
			return a.Target.PrefixSetName < b.Target.PrefixSetName
		}
		return actionPrecedence(a.Kind) < actionPrecedence(b.Kind)
	})

	return actions
}

func ipSetsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for ip := range a {
		if _, exists := b[ip]; !exists {
			return false
		}
	}
	return true
}

func sortedIPs(ips map[string]struct{}) []string {
	out := make([]string, 0, len(ips))
	for ip := range ips {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func actionPrecedence(kind ActionKind) int {
	switch kind {
	case CreatePrefixSet:
		return 0
	case UpdatePrefixSet:
		return 1
	case DeletePrefixSet:
		return 2
	default:
		return 3
	}
}
