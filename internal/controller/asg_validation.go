package controller

import (
	"fmt"

	v1alpha1 "github.com/Azure/pod-nsg-controller/api/v1alpha1"
	"github.com/Azure/pod-nsg-controller/internal/model"
)

// ASGValidationResult holds parsed references and validation errors grouped by mapping index.
type ASGValidationResult struct {
	ParsedByMapping  map[int][]model.ParsedASGReference
	ValidationErrors map[int][]string
}

// HasErrors returns true if any mapping has validation errors.
func (r ASGValidationResult) HasErrors() bool {
	for _, errs := range r.ValidationErrors {
		if len(errs) > 0 {
			return true
		}
	}
	return false
}

// validateASGResourceIDs validates all ASG resource IDs in the spec and returns
// parsed references and any validation errors, grouped by mapping index.
func validateASGResourceIDs(spec v1alpha1.PodASGMappingSpec) ASGValidationResult {
	result := ASGValidationResult{
		ParsedByMapping:  make(map[int][]model.ParsedASGReference),
		ValidationErrors: make(map[int][]string),
	}

	for i, mapping := range spec.Mappings {
		for j, asgRef := range mapping.ApplicationSecurityGroups {
			parsed, err := model.ParseASGResourceID(asgRef.ResourceID)
			if err != nil {
				errMsg := fmt.Sprintf("mapping[%d].applicationSecurityGroups[%d].resourceId %q: %v",
					i, j, asgRef.ResourceID, err)
				result.ValidationErrors[i] = append(result.ValidationErrors[i], errMsg)
			} else {
				result.ParsedByMapping[i] = append(result.ParsedByMapping[i], parsed)
			}
		}
	}

	return result
}

// canonicalizeSpecWithParsed rewrites the spec's resource IDs using the canonical
// form from parsed references. Invalid resource IDs are left unchanged.
func canonicalizeSpecWithParsed(
	spec v1alpha1.PodASGMappingSpec,
	parsedByMapping map[int][]model.ParsedASGReference,
) v1alpha1.PodASGMappingSpec {
	newMappings := make([]v1alpha1.Mapping, len(spec.Mappings))
	for i, mapping := range spec.Mappings {
		newASGs := make([]v1alpha1.ASGReference, len(mapping.ApplicationSecurityGroups))
		copy(newASGs, mapping.ApplicationSecurityGroups)

		parsed := parsedByMapping[i]
		canonicalByResourceID := make(map[string]string, len(parsed))
		for _, parsedRef := range parsed {
			canonicalByResourceID[canonicalASGKey(parsedRef.FullResourceID)] = parsedRef.FullResourceID
		}

		for j, asgRef := range mapping.ApplicationSecurityGroups {
			if canonicalResourceID, ok := canonicalByResourceID[canonicalASGKey(asgRef.ResourceID)]; ok {
				newASGs[j].ResourceID = canonicalResourceID
			}
		}

		newMappings[i] = v1alpha1.Mapping{
			PodSelector:               mapping.PodSelector,
			ApplicationSecurityGroups: newASGs,
		}
	}

	return v1alpha1.PodASGMappingSpec{
		Mappings: newMappings,
	}
}
