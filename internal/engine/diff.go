package engine

import (
	"os"
	"sort"
	"strconv"
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

	thresholdPercent := patchThresholdPercentFromEnv()

	for key, de := range desiredByKey {
		if ae, exists := actualByKey[key]; exists {
			if !ipSetsEqual(de.ips, ae.ips) {
				addIPs, removeIPs := computeIPDelta(de.ips, ae.ips)
				if shouldUsePatch(len(addIPs), len(removeIPs), len(de.ips), len(ae.ips), thresholdPercent) {
					actions = append(actions, Action{
						Kind:       PatchPrefixSet,
						Target:     de.target,
						DesiredIPs: sortedIPs(de.ips),
						AddIPs:     addIPs,
						RemoveIPs:  removeIPs,
					})
				} else {
					actions = append(actions, Action{
						Kind:       UpdatePrefixSet,
						Target:     de.target,
						DesiredIPs: sortedIPs(de.ips),
					})
				}
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
	case UpdatePrefixSet, PatchPrefixSet:
		return 1
	case DeletePrefixSet:
		return 2
	default:
		return 3
	}
}

// computeIPDelta returns sorted lists of IPs to add and remove.
func computeIPDelta(desiredIPs, actualIPs map[string]struct{}) (add []string, remove []string) {
	for ip := range desiredIPs {
		if _, exists := actualIPs[ip]; !exists {
			add = append(add, ip)
		}
	}
	for ip := range actualIPs {
		if _, exists := desiredIPs[ip]; !exists {
			remove = append(remove, ip)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

// shouldUsePatch returns true if the delta is small enough to use patch.
// Formula: patch when 100*deltaOps <= thresholdPercent*denominator.
func shouldUsePatch(addCount, removeCount, desiredCount, actualCount, thresholdPercent int) bool {
	deltaOps := addCount + removeCount
	denominator := desiredCount + actualCount
	if denominator == 0 {
		return false
	}
	return 100*deltaOps <= thresholdPercent*denominator
}

// patchThresholdPercentFromEnv reads the patch threshold from environment.
// Default is 50. Valid range: 1..100; invalid/missing → 50.
func patchThresholdPercentFromEnv() int {
	const defaultThreshold = 50
	val := os.Getenv("POD_NSG_PATCH_THRESHOLD_PERCENT")
	if val == "" {
		return defaultThreshold
	}
	v, err := strconv.Atoi(val)
	if err != nil || v < 1 || v > 100 {
		return defaultThreshold
	}
	return v
}
